// Package e2e provides end-to-end tests that exercise a real orca server against a
// real Docker daemon.
//
// These tests are the only place orca's layers are exercised together as an operator
// uses them: a manifest goes in through the client and containers come out on the
// daemon. They deliberately cover whole journeys rather than individual behaviours —
// the unit tests own the edge cases — and they are what catches the mistakes that
// only appear when the real runtime is involved, such as a container removal racing
// the shutdown it was meant to follow.
//
// The suite is not parallel, and cannot be. Every server shares one Docker daemon,
// and the reconciler stops any orca-labelled container that no workload asks for, so
// two servers running at once would tear down each other's work.
package e2e_test

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

const (
	// The image the tests run. Small, quick to start, and long-running, so a
	// workload built from it stays up until orca stops it.
	testImage = "nginx:1.27-alpine"
	// How long to wait for the server to converge on a desired state. Generous
	// because the first workload to want the image pays to pull it, which is part
	// of what these tests are checking.
	convergeTimeout = 2 * time.Minute
)

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	suite.Run(t, new(Suite))
}

// TestWorkloadLifecycle covers the whole journey an operator takes with a workload:
// applying it, watching it start, reaching it, re-applying it, reading its output and
// finally deleting it.
func (s *Suite) TestWorkloadLifecycle() {
	name := s.workloadName()
	spec := s.containerSpec(name, manifest.Port{To: 80, From: 8180})

	workload, created, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.True(created)
	s.Equal(name, workload.Name)
	s.Equal(1, workload.Version)

	started := s.awaitState(name, "running")
	s.Require().Len(started.Instances, 1)
	s.Equal("running", started.Instances[0].State)
	s.NotEmpty(started.Instances[0].SpecHash)

	// The point of a port mapping is that something outside the container can reach
	// the process, which only a real connection proves.
	s.awaitListening("127.0.0.1:8180")

	// An unchanged specification must not bump the version or restart healthy work,
	// which is what makes re-running apply safe.
	reapplied, created, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.False(created)
	s.Equal(workload.Version, reapplied.Version)
	s.Equal(started.Instances[0].ID, s.instanceID(name))

	// nginx announces itself on startup, so finding it here proves the log stream is
	// demultiplexed rather than returned as raw framed bytes.
	logs, err := s.client.Logs(s.ctx(), name, 50)
	s.Require().NoError(err)
	s.Contains(logs, "nginx")

	workloads, err := s.client.List(s.ctx())
	s.Require().NoError(err)
	s.Contains(s.names(workloads), name)

	deleted, err := s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)
	s.True(deleted.Deleting)

	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
	s.Empty(s.containers(name))
}

// TestListQuery covers filtering the list by a query into the stored specification,
// which is how an operator finds workloads without knowing their names.
func (s *Suite) TestListQuery() {
	web, api := s.workloadName()+"-web", s.workloadName()+"-api"
	s.T().Cleanup(func() { s.cleanup(web) })
	s.T().Cleanup(func() { s.cleanup(api) })

	first := s.containerSpec(web, manifest.Port{To: 80})
	first.Labels = map[string]string{"app": "web", "env": "prod"}

	second := s.containerSpec(api, manifest.Port{To: 80})
	second.Labels = map[string]string{"app": "api", "env": "prod"}

	_, _, err := s.client.Apply(s.ctx(), first)
	s.Require().NoError(err)
	_, _, err = s.client.Apply(s.ctx(), second)
	s.Require().NoError(err)

	// A label is reachable like any other part of the specification.
	matched, err := s.client.List(s.ctx(), "$.labels.app=web")
	s.Require().NoError(err)
	s.Equal([]string{web}, s.names(matched))

	// Queries are combined, so adding one narrows rather than widens.
	both, err := s.client.List(s.ctx(), "$.labels.env=prod")
	s.Require().NoError(err)
	s.Len(both, 2)

	narrowed, err := s.client.List(s.ctx(), "$.labels.env=prod", "$.labels.app=api")
	s.Require().NoError(err)
	s.Equal([]string{api}, s.names(narrowed))

	// The query reaches past labels into the rest of the specification.
	byImage, err := s.client.List(s.ctx(), "$.container.image="+testImage)
	s.Require().NoError(err)
	s.Len(byImage, 2)

	// A number in the specification is matched by its digits, since a caller only
	// ever has strings to hand.
	byPort, err := s.client.List(s.ctx(), "$.container.ports[0].to=80")
	s.Require().NoError(err)
	s.Len(byPort, 2)

	// Nothing matching is an empty result, not an error.
	none, err := s.client.List(s.ctx(), "$.labels.app=nope")
	s.Require().NoError(err)
	s.Empty(none)

	// A malformed query is the caller's mistake and has to be reported as such.
	_, err = s.client.List(s.ctx(), "$.labels.app")
	s.True(client.IsBadRequest(err), "expected a bad request error, got %v", err)
}

// TestDynamicPortIsAllocated covers the usual case for a port: the manifest names
// only the port inside the container and orca picks the host port that reaches it.
func (s *Suite) TestDynamicPortIsAllocated() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// No `from`, so the server has to choose one.
	workload, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80}))
	s.Require().NoError(err)
	s.Require().Len(workload.Ports, 1)

	allocated := workload.Ports[0]
	s.Equal(80, allocated.To)
	s.True(allocated.Dynamic)
	s.NotZero(allocated.From)

	// An allocated port is only useful if it actually reaches the container.
	s.awaitState(name, "running")
	s.awaitListening(fmt.Sprintf("127.0.0.1:%d", allocated.From))

	// The allocation is sticky: a change that leaves the port list alone must not
	// move the address, or anything pointing at it would break on an image bump.
	changed := s.containerSpec(name, manifest.Port{To: 80})
	changed.Container.Env = map[string]string{"EXAMPLE": "CHANGED"}

	updated, _, err := s.client.Apply(s.ctx(), changed)
	s.Require().NoError(err)
	s.Require().Len(updated.Ports, 1)
	s.Equal(allocated.From, updated.Ports[0].From)
}

// TestFixedPortConflictIsRejected covers a host port that another workload already
// holds, which the operator asked for explicitly and so must hear about at once.
func (s *Suite) TestFixedPortConflictIsRejected() {
	first, second := s.workloadName()+"-a", s.workloadName()+"-b"
	s.T().Cleanup(func() { s.cleanup(first) })
	s.T().Cleanup(func() { s.cleanup(second) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(first, manifest.Port{To: 80, From: 8185}))
	s.Require().NoError(err)

	// The same host port for a different workload cannot be honoured, and saying so
	// now is far better than accepting it and never starting the container.
	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(second, manifest.Port{To: 80, From: 8185}))
	s.True(client.IsConflict(err), "expected a conflict error, got %v", err)
}

// TestWorkloadReplacedWhenSpecChanges covers a specification change, which docker
// cannot apply to a running container and so has to be a replacement.
func (s *Suite) TestWorkloadReplacedWhenSpecChanges() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80, From: 8181}))
	s.Require().NoError(err)

	s.awaitState(name, "running")
	first := s.instanceID(name)

	changed := s.containerSpec(name, manifest.Port{To: 80, From: 8181})
	changed.Container.Env = map[string]string{"EXAMPLE": "CHANGED"}

	workload, created, err := s.client.Apply(s.ctx(), changed)
	s.Require().NoError(err)
	s.False(created)
	s.Equal(2, workload.Version)

	// The replacement has to be a different container, and has to come up.
	s.awaitInstanceOtherThan(name, first)
}

// TestWorkloadRestartedAfterItDies covers recovery from a workload dying, which orca
// owns rather than delegating to docker's restart policy.
func (s *Suite) TestWorkloadRestartedAfterItDies() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80, From: 8182}))
	s.Require().NoError(err)

	s.awaitState(name, "running")
	original := s.instanceID(name)

	// Killing the container is the closest thing to the workload crashing.
	s.Require().NoError(exec.Command("docker", "kill", original).Run())

	s.awaitInstanceOtherThan(name, original)
}

// TestWorkloadAdoptedAfterServerRestart covers a server restart, which the
// desired-state-only database makes possible without duplicating work.
func (s *Suite) TestWorkloadAdoptedAfterServerRestart() {
	name := s.workloadName()

	// The first server's data has to outlive it, so the restarted one reads the same
	// desired state.
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80, From: 8183}))
	s.Require().NoError(err)

	s.awaitState(name, "running")
	original := s.instanceID(name)

	// The container outlives the server, and nothing about it is persisted, so a
	// restarted server has to rediscover it from its labels.
	s.restart(withDataDirectory(directory))
	s.T().Cleanup(func() { s.cleanup(name) })

	workload := s.awaitState(name, "running")
	s.Require().Len(workload.Instances, 1)

	// Adopting means the same container, not a replacement: a server that started a
	// second one would be running the workload twice.
	s.Equal(original, workload.Instances[0].ID)
	s.Len(s.containers(name), 1)
}

// TestOrphanedContainerIsStopped covers what a delete performed while the server was
// down leaves behind: a container orca owns that no workload asks for.
func (s *Suite) TestOrphanedContainerIsStopped() {
	name := s.workloadName()

	s.Require().NoError(exec.Command("docker", "run", "--detach",
		"--name", "orca-"+name+"-orphan",
		"--label", "orca.workload="+name,
		"--label", "orca.spec-hash=deadbeef",
		"--label", "orca.version=1",
		testImage,
	).Run())

	s.Require().Eventuallyf(func() bool {
		return len(s.containers(name)) == 0
	}, convergeTimeout, 500*time.Millisecond, "orphaned container was never stopped")
}

// TestUnsupportedRuntimeIsRejected covers the runtime the API describes but no driver
// implements, which must be refused rather than accepted and never run.
func (s *Suite) TestUnsupportedRuntimeIsRejected() {
	_, _, err := s.client.Apply(s.ctx(), manifest.Spec{
		Version: "v1",
		Name:    s.workloadName(),
		Script:  &manifest.Script{Raw: `echo "hello world"`},
	})

	s.True(client.IsUnprocessable(err), "expected an unprocessable error, got %v", err)
}

// TestApplyDuringTeardownIsRejected covers re-applying a workload that is still being
// torn down, which would otherwise race the removal.
func (s *Suite) TestApplyDuringTeardownIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80, From: 8184})

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, "running")

	// Delete returns as soon as the intent is recorded, so the workload is still on
	// its way out here.
	deleted, err := s.client.Delete(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().True(deleted.Deleting)

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.True(client.IsConflict(err), "expected a conflict error, got %v", err)
}

// TestMissingWorkload covers the not-found path on every endpoint that takes a name.
func (s *Suite) TestMissingWorkload() {
	_, err := s.client.Get(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	_, err = s.client.Delete(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	_, err = s.client.Logs(s.ctx(), "does-not-exist", 10)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}
