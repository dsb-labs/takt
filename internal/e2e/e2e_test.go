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
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/dsb-labs/orca/internal/restore"
	execdriver "github.com/dsb-labs/orca/internal/server/driver/exec"
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

// TestMain lets this test binary act as a confinement trampoline.
//
// The server runs inside the test process, so the binary it executes to start a
// confined exec workload is this one. Without this the workload would run the suite a
// second time instead of confining itself and becoming the command.
//
// The server the suite runs also assumes a delegated cgroup subtree, so it can
// enforce resource limits on exec workloads rather than refuse them. That is a
// property of how the suite was started: run it through "make e2e" or
// scripts/delegated.sh, which grant one.
func TestMain(m *testing.M) {
	execdriver.Confine()

	os.Exit(m.Run())
}

func TestEndToEnd(t *testing.T) {
	// The suite needs a docker daemon and takes minutes rather than seconds, so the
	// main CI workflow leaves it out and the e2e workflow runs it. Saying so here is
	// what tells somebody reading a log that the suite was left out on purpose.
	if testing.Short() {
		t.Skip("end-to-end tests need a docker daemon: run without -short")
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

	started := s.awaitState(name, client.WorkloadStateRunning)
	s.Require().Len(started.Instances, 1)
	s.Equal(client.InstanceStateRunning, started.Instances[0].State)
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
	var logs strings.Builder
	s.Require().NoError(s.client.Logs(s.ctx(), &logs, name, client.WithTail(50)))
	s.Contains(logs.String(), "nginx")

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
	byPort, err := s.client.List(s.ctx(), "$.ports[0].to=80")
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
	s.awaitState(name, client.WorkloadStateRunning)
	s.awaitListening(fmt.Sprintf("127.0.0.1:%d", allocated.From))

	// The allocation is sticky: a change that leaves the port list alone must not
	// move the address, or anything pointing at it would break on an image bump.
	changed := s.containerSpec(name, manifest.Port{To: 80})
	changed.Env = map[string]string{"EXAMPLE": "CHANGED"}

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

// TestPortProtocols covers publishing one port over both protocols, which is what a
// workload speaking DNS needs and what nothing before could ask for.
func (s *Suite) TestPortProtocols() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// The same host port on both protocols is one workload holding two unrelated
	// ports, which only a schema keying an allocation by protocol accepts.
	applied, _, err := s.client.Apply(s.ctx(), s.containerSpec(name,
		manifest.Port{To: 80, From: 8188, Protocol: manifest.ProtocolTCP},
		manifest.Port{To: 80, From: 8188, Protocol: manifest.ProtocolUDP},
	))
	s.Require().NoError(err)

	s.Require().Len(applied.Ports, 2)
	s.Equal("tcp", applied.Ports[0].Protocol)
	s.Equal("udp", applied.Ports[1].Protocol)
	s.Equal(8188, applied.Ports[0].From)
	s.Equal(8188, applied.Ports[1].From)

	s.awaitState(name, client.WorkloadStateRunning)

	// Reading the publication back from docker is what proves the driver asked for
	// the protocol rather than publishing TCP twice.
	published := s.publishedPorts(name)
	s.Contains(published, "80/tcp")
	s.Contains(published, "80/udp")

	// The container really is reachable over TCP, which the UDP mapping must not
	// have displaced.
	s.awaitListening("127.0.0.1:8188")
}

// TestWorkloadReplacedWhenSpecChanges covers a specification change, which docker
// cannot apply to a running container and so has to be a replacement.
func (s *Suite) TestWorkloadReplacedWhenSpecChanges() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80, From: 8181}))
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
	first := s.instanceID(name)

	changed := s.containerSpec(name, manifest.Port{To: 80, From: 8181})
	changed.Env = map[string]string{"EXAMPLE": "CHANGED"}

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

	s.awaitState(name, client.WorkloadStateRunning)
	original := s.instanceID(name)

	// Killing the container is the closest thing to the workload crashing.
	s.Require().NoError(exec.Command("docker", "kill", original).Run())

	s.awaitInstanceOtherThan(name, original)
}

// TestStoppedWorkloadStaysDown covers the lifecycle commands: a stop holds the
// workload down across passes and a server restart, and a start resumes the same
// version.
func (s *Suite) TestStoppedWorkloadStaysDown() {
	name := s.workloadName()

	// The first server's data has to outlive it, so the restarted one below reads
	// the same desired state.
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))
	s.T().Cleanup(func() { s.cleanup(name) })

	applied, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80, From: 8186}))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	stopped, err := s.client.Stop(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)
	s.True(stopped.Suspended)
	s.Equal(client.WorkloadStateSuspended, stopped.State)

	// The stopped instance is still reported so its output stays readable, but
	// nothing is up any more.
	for _, instance := range stopped.Instances {
		s.NotEqual(client.InstanceStateRunning, instance.State)
	}

	// A stop that merely killed the container would be undone within one reconcile
	// interval. Outlasting several passes is what proves the suspension is desired
	// state rather than a one-shot kill.
	s.Never(func() bool {
		workload, err := s.client.Get(s.ctx(), name)
		if err != nil || workload.State != client.WorkloadStateSuspended {
			return true
		}

		for _, instance := range workload.Instances {
			if instance.State == client.InstanceStateRunning {
				return true
			}
		}

		return false
	}, 3*time.Second, 500*time.Millisecond, "a stopped workload came back up")

	// The instance is stopped rather than discarded, so what the workload last did
	// stays readable while it is down.
	var logs strings.Builder
	s.Require().NoError(s.client.Logs(s.ctx(), &logs, name, client.WithTail(50)))
	s.NotEmpty(logs.String())

	// A workload stopped before an upgrade must still be stopped after it, or the
	// mark is useless for the case it exists for.
	s.restart(withDataDirectory(directory))
	s.awaitState(name, client.WorkloadStateSuspended)

	started, err := s.client.Start(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)
	s.False(started.Suspended)

	// The specification never changed, so the resume runs the same version rather
	// than a replacement.
	s.Equal(client.WorkloadStateRunning, started.State)
	s.Equal(applied.Version, started.Version)
}

// TestRestartReplacesTheInstance covers the operator-requested restart, which
// replaces the instances without the specification moving.
func (s *Suite) TestRestartReplacesTheInstance() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	applied, _, err := s.client.Apply(s.ctx(), s.containerSpec(name, manifest.Port{To: 80, From: 8187}))
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
	original := s.instanceID(name)

	restarted, err := s.client.Restart(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// The waiting restart returns once an instance the request never saw is up.
	s.Require().Len(restarted.Instances, 1)
	s.NotEqual(original, restarted.Instances[0].ID)

	// The specification never changed, so the version does not move.
	s.Equal(applied.Version, restarted.Version)
}

// TestStopRemovesMountedValues covers what suspension does to the disk: a stopped
// workload has no reader for the values it mounted, so their plaintext is removed
// rather than sitting there for the life of the suspension.
func (s *Suite) TestStopRemovesMountedValues() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("plaintext"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// The value is on the disk while the workload runs — that is what a mount is.
	mounted, err := filepath.Glob(filepath.Join(s.directory, "mounts", "files", "*"))
	s.Require().NoError(err)
	s.Require().NotEmpty(mounted)

	_, err = s.client.Stop(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// The wait returns when nothing is running, and the removal happens on the pass
	// that observes that, so the files may be a pass behind the stop.
	s.Require().Eventuallyf(func() bool {
		remaining, err := filepath.Glob(filepath.Join(s.directory, "mounts", "files", "*"))

		return err == nil && len(remaining) == 0
	}, convergeTimeout, 500*time.Millisecond, "stopping a workload left its mounted values on the disk")
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

	s.awaitState(name, client.WorkloadStateRunning)
	original := s.instanceID(name)

	// The container outlives the server, and nothing about it is persisted, so a
	// restarted server has to rediscover it from its labels.
	s.restart(withDataDirectory(directory))
	s.T().Cleanup(func() { s.cleanup(name) })

	workload := s.awaitState(name, client.WorkloadStateRunning)
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

	// Removed here as well as by orca. The test asserts orca reaps it, so a failure
	// leaves it behind — and the name is derived from the test, so the container would
	// then collide with the next run of it. Docker reports that as an exit status
	// rather than as a message, which is a poor thing to debug from.
	s.T().Cleanup(func() { s.cleanup(name) })

	container := "orca-" + name + "-orphan"

	// Any output docker produces is captured, because the exit status alone says
	// nothing about why: a name conflict and a missing image look the same.
	create := exec.Command("docker", "run", "--detach",
		"--name", container,
		"--label", "orca.workload="+name,
		"--label", "orca.spec-hash=deadbeef",
		"--label", "orca.version=1",
		testImage,
	)

	out, err := create.CombinedOutput()
	s.Require().NoErrorf(err, "failed to create the orphaned container: %s", out)

	s.Require().Eventuallyf(func() bool {
		return len(s.containers(name)) == 0
	}, convergeTimeout, 500*time.Millisecond, "orphaned container was never stopped")
}

// TestHealthyWorkloadIsReported covers a workload whose check passes, which must
// read as healthy and be left running.
func (s *Suite) TestHealthyWorkloadIsReported() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80})
	// nginx serves its index at the root, so this is a check the workload passes.
	//
	// The timeout and retries are deliberately slack: this test is about a passing
	// check being reported and acted on, not about tight timings, and a probe that
	// merely lost a race with a loaded machine must not read as a workload failure.
	spec.Health = &manifest.Health{
		HTTP:     "/",
		Interval: 5 * time.Second,
		Timeout:  5 * time.Second,
		Retries:  10,
	}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	workload := s.awaitHealth(name, client.HealthHealthy)
	s.Require().Len(workload.Instances, 1)

	reported := workload.Instances[0].Health
	s.Require().NotNil(reported)
	s.Equal(client.HealthHealthy, reported.Status)
	s.Empty(reported.Error)
	s.False(reported.CheckedAt.IsZero())

	// A passing check must leave the workload alone: the instance it is running is
	// the one it started with.
	//
	// Polled inline rather than with Never, whose condition runs on a goroutine that
	// outlives the assertion — it would still be reading the suite's client while the
	// next test's teardown replaced it.
	instance := workload.Instances[0].ID
	for range 5 {
		time.Sleep(time.Second)

		current, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(current.Instances, 1)
		s.Equal(instance, current.Instances[0].ID, "a workload passing its check was replaced")
	}
}

// TestUnhealthyWorkloadIsReplaced covers a workload the runtime reports as running
// but which fails its check, which is the entire reason for checking it.
func (s *Suite) TestUnhealthyWorkloadIsReplaced() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80})
	// nginx answers 404 for a path it does not serve, so the container is up and
	// listening while the check fails — exactly the case docker's own health support
	// reports but never acts on.
	spec.Health = &manifest.Health{
		HTTP:     "/nothing-is-served-here",
		Interval: time.Second,
		Timeout:  time.Second,
		Retries:  2,
	}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	original := s.awaitInstance(name)

	workload := s.awaitHealth(name, client.HealthUnhealthy)
	s.Require().Len(workload.Instances, 1)

	reported := workload.Instances[0].Health
	s.Require().NotNil(reported)
	s.Contains(reported.Error, "404")
	s.NotZero(*reported.Failures)

	// The workload is failed rather than running, so it takes the same paced restart
	// a crashed container does — and keeps being replaced while it cannot serve.
	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)
		if err != nil || len(workload.Instances) == 0 {
			return false
		}

		return workload.Instances[0].ID != original
	}, convergeTimeout, 500*time.Millisecond, "workload %q never replaced instance %s", name, original)
}

// TestWarmingWorkloadIsNotReplaced covers the start period, which exists so that a
// workload slow to become ready is not killed for failing checks it was never going
// to pass yet.
func (s *Suite) TestWarmingWorkloadIsNotReplaced() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80})
	spec.Health = &manifest.Health{
		HTTP:     "/nothing-is-served-here",
		Interval: time.Second,
		Timeout:  time.Second,
		Retries:  1,
		// Long enough that the check cannot exhaust its retries within the window
		// this test observes.
		StartPeriod: 5 * time.Minute,
	}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	original := s.awaitInstance(name)

	// Failures inside the start period are expected rather than meaningful, so the
	// workload stays as it is however many of them accumulate.
	//
	// Polled inline rather than with Never, whose condition runs on a goroutine that
	// outlives the assertion and would still be reading the suite's client once the
	// next test replaced it.
	for range 15 {
		time.Sleep(time.Second)

		current, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(current.Instances, 1)
		s.Equal(original, current.Instances[0].ID, "a workload inside its start period was replaced")
	}

	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(workload.Instances, 1)

	reported := workload.Instances[0].Health
	s.Require().NotNil(reported)
	s.Equal(client.HealthStarting, reported.Status)
	// It is failing, and being given the chance to stop.
	s.NotZero(*reported.Failures)
}

// TestDefaultPolicyStillRestarts covers the compatibility claim. A manifest that says
// nothing about restarting behaves as it did before the policy existed.
func (s *Suite) TestDefaultPolicyStillRestarts() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// No restart stanza at all, and a command that ends cleanly. Under the default the
	// clean exit is not a reason to stop, so orca brings it back.
	spec := s.jobSpec(name, "", 0)
	spec.Restart = nil

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	original := s.awaitInstance(name)

	s.Require().Eventuallyf(func() bool {
		current, err := s.client.Get(s.ctx(), name)
		if err != nil || len(current.Instances) == 0 {
			return false
		}

		return current.Instances[0].ID != original
	}, convergeTimeout, 500*time.Millisecond, "a workload with no restart policy was not restarted")
}

// TestCompletedJobIsNotRestarted covers a workload that finishes, which is what the
// restart policy exists for: the runtime reports it gone, and orca must leave it gone.
func (s *Suite) TestCompletedJobIsNotRestarted() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.jobSpec(name, manifest.RestartOnFailure, 0))
	s.Require().NoError(err)

	workload := s.awaitState(name, client.WorkloadStateCompleted)
	s.Require().Len(workload.Instances, 1)
	s.Equal(client.InstanceStateCompleted, workload.Instances[0].State)

	// The instance that ran stays the instance that ran. Anything else means orca
	// restarted work nobody asked it to repeat.
	instance := workload.Instances[0].ID
	for range 8 {
		time.Sleep(time.Second)

		current, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(current.Instances, 1)
		s.Equal(instance, current.Instances[0].ID, "a completed workload was restarted")
		s.Equal(client.WorkloadStateCompleted, current.State)
	}
}

// TestFailedJobIsRestarted covers the other half of on-failure: a job that did not
// succeed is retried, on the same paced schedule a crashed service takes.
func (s *Suite) TestFailedJobIsRestarted() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.jobSpec(name, manifest.RestartOnFailure, 1))
	s.Require().NoError(err)

	original := s.awaitInstance(name)

	// A non-zero exit is exactly what this policy restarts, so the instance that ran
	// is replaced rather than left as it is.
	s.Require().Eventuallyf(func() bool {
		current, err := s.client.Get(s.ctx(), name)
		if err != nil || len(current.Instances) == 0 {
			return false
		}

		return current.Instances[0].ID != original
	}, convergeTimeout, 500*time.Millisecond, "a failed job was never retried")
}

// TestRestartsArePaced covers the backoff through the whole stack, which is what
// stops a workload that cannot start from spinning the reconciler and the daemon.
//
// The reconciler's own tests drive this against a clock they control. What they cannot
// show is that a real container exiting immediately is paced at all: it is observed as
// running on its way through, and treating that as convergence cleared the backoff on
// every pass and let such a workload restart as fast as docker could be asked. Only a
// real daemon produces that sighting.
func (s *Suite) TestRestartsArePaced() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// Exits the moment it starts, so every pass finds it dead and wants to run it
	// again. The delay is what the pacing is built from, and doubles each time: one
	// restart is immediate, the next waits two seconds, then four.
	spec := s.jobSpec(name, manifest.RestartAlways, 1)
	spec.Restart.Delay = time.Second

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	// Counted by identifier rather than by container, because a restart clears the
	// corpse before it starts the replacement: however fast the workload loops, only
	// one container exists at a time and counting them would report one either way.
	seen := make(map[string]struct{})

	// Long enough for dozens of unpaced restarts, since the reconciler ticks every
	// second and each container ending prompts a pass of its own. Under the backoff it
	// fits the immediate restart and the two-second wait after it.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)

		for _, instance := range workload.Instances {
			seen[instance.ID] = struct{}{}
		}

		time.Sleep(100 * time.Millisecond)
	}

	s.LessOrEqualf(len(seen), 6,
		"a workload exiting at once ran %d instances, so the backoff is not pacing it", len(seen))

	// The other half of the claim. A workload being paced is still being retried, and
	// one that stopped entirely would also pass the check above.
	s.Greaterf(len(seen), 1, "a workload under always was not restarted at all")
}

// TestGivesUpAfterTheAttemptsAllowed covers the end of the backoff: a workload that
// caps its attempts is eventually left alone rather than retried forever.
func (s *Suite) TestGivesUpAfterTheAttemptsAllowed() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// Two attempts and no wait worth speaking of, so the cap is reached quickly and
	// is the only thing that can stop the retries.
	spec := s.jobSpec(name, manifest.RestartAlways, 1)
	spec.Restart.Attempts = 2
	spec.Restart.Delay = 100 * time.Millisecond

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	instance := s.awaitInstance(name)

	// The attempts are spent well inside this. Counted by identifier for the reason
	// the paced test counts that way: only one container exists at a time.
	seen := map[string]struct{}{instance: {}}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)

		for _, current := range workload.Instances {
			seen[current.ID] = struct{}{}
		}

		time.Sleep(100 * time.Millisecond)
	}

	// Two attempts on top of the instance the workload started with, so a third
	// restart is one the cap should have stopped.
	s.LessOrEqualf(len(seen), 3, "a workload capped at two attempts ran %d instances", len(seen))

	settled := len(seen)

	// Nothing further is started once the cap is reached, which is what distinguishes
	// giving up from waiting: a paced workload keeps producing new instances, just
	// more slowly.
	for range 5 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)

		for _, current := range workload.Instances {
			seen[current.ID] = struct{}{}
		}

		s.Require().Lenf(seen, settled, "a workload past its attempt cap was restarted again")
	}
}

// TestNeverPolicyKeepsAFailureVisible covers a workload retired without being called a
// success, which is the distinction between what a policy decides and how a workload
// ended.
func (s *Suite) TestNeverPolicyKeepsAFailureVisible() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.jobSpec(name, manifest.RestartNever, 1))
	s.Require().NoError(err)

	// Reported as failed rather than completed. The policy says not to run it again,
	// and the exit code says it did not do its job — an operator needs both.
	workload := s.awaitState(name, client.WorkloadStateFailed)
	s.Require().Len(workload.Instances, 1)
	s.Equal(client.InstanceStateFailed, workload.Instances[0].State)

	instance := workload.Instances[0].ID
	for range 8 {
		time.Sleep(time.Second)

		current, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(current.Instances, 1)
		s.Equal(instance, current.Instances[0].ID, "a workload under never was restarted")
	}
}

// TestNeverPullPolicyRefusesAnAbsentImage covers the pull policy's loud failure: a
// workload forbidden to pull must not fall through to a pull when its image is
// absent, or the policy is indistinguishable from missing.
func (s *Suite) TestNeverPullPolicyRefusesAnAbsentImage() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// An image no host holds: the tag does not exist, so a fall-through to a pull
	// would fail this test through the timeout below rather than silently pass it.
	spec := s.containerSpec(name)
	spec.Container.Image = "orca-e2e/does-not-exist:latest"
	spec.Container.Pull = manifest.PullNever

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// The apply is accepted — the policy fails the start, not the write — so the
	// workload sits in paced restarts without ever producing a container.
	for range 8 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Equal(client.WorkloadStatePending, workload.State)
		s.Empty(s.containers(name), "a workload under pull never created a container")
	}

	// A workload that will not converge says why, which is what tells this pending
	// apart from one that is merely slow to start.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.NotEmpty(workload.LastError, "a failing workload reported no reason")
	s.False(workload.LastErrorAt.IsZero(), "a failing workload reported no failure time")
}

// TestNeverPullPolicyRunsAPresentImage covers the policy's other half: an image that
// is already on the host starts normally, since never forbids pulling rather than
// running.
func (s *Suite) TestNeverPullPolicyRunsAPresentImage() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// Pulled here rather than assumed, so the test holds up when run on its own
	// instead of depending on an earlier test having wanted the image.
	s.Require().NoError(exec.Command("docker", "pull", testImage).Run())

	spec := s.containerSpec(name)
	spec.Container.Pull = manifest.PullNever

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
}

// TestChangingASpecRerunsACompletedJob covers the re-run trigger. A finished job runs
// again when the operator changes what they asked for, and not otherwise.
func (s *Suite) TestChangingASpecRerunsACompletedJob() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.jobSpec(name, manifest.RestartOnFailure, 0)

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	first := s.awaitState(name, client.WorkloadStateCompleted)
	s.Require().Len(first.Instances, 1)
	original := first.Instances[0].ID

	// Re-applying the same specification changes nothing, so the job stays finished.
	// This is what stops a repeated apply running a job over and over.
	unchanged, created, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.Version, unchanged.Version)

	time.Sleep(5 * time.Second)

	still, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(still.Instances, 1)
	s.Equal(original, still.Instances[0].ID, "an unchanged apply re-ran a completed job")

	// A changed specification is a new thing to run, and what already ran is out of
	// date. That is the same rule that replaces a running service on an image bump.
	spec.Env = map[string]string{"EXAMPLE": "CHANGED"}

	updated, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.Greater(updated.Version, first.Version)

	s.Require().Eventuallyf(func() bool {
		current, err := s.client.Get(s.ctx(), name)
		if err != nil || len(current.Instances) == 0 {
			return false
		}

		return current.Instances[0].ID != original
	}, convergeTimeout, 500*time.Millisecond, "a changed specification did not re-run the job")
}

// TestExecJobRunsAndCompletes covers the exec runtime's simplest case: a command that
// does something and ends.
func (s *Suite) TestExecJobRunsAndCompletes() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", "echo did-the-work; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartOnFailure}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	workload := s.awaitState(name, client.WorkloadStateCompleted)
	s.Require().Len(workload.Instances, 1)
	s.Equal(client.InstanceStateCompleted, workload.Instances[0].State)

	// The command's output is captured on disk, which is what logs reads for a runtime
	// with no daemon to ask.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "did-the-work")
}

// TestExecWorkloadRunsUnderItsResourceLimits covers the exec runtime's resource
// limits end to end: the section is accepted through the API, the workload runs, and
// the kernel refuses it what the limit denies.
func (s *Suite) TestExecWorkloadRunsUnderItsResourceLimits() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// Two processes: the shell and one child. The loop asks for more, and the shell
	// reports each refused fork on stderr, which lands in the workload's log. The
	// report is what is awaited, because which wording a shell prints varies but
	// every one of them names the fork.
	//
	// The script forks nothing until it reads the limit from its own cgroup,
	// because the limit is written as the command starts and a fast shell can fork
	// ahead of it — the driver's trampoline allowance covers that moment. Builtins
	// only until then: reading through redirection forks nothing, so the check
	// cannot be refused by the limit it waits for.
	spec := s.execSpec(name, "sh", "-c",
		`read line < /proc/self/cgroup
		limit="/sys/fs/cgroup${line#0::}/pids.max"
		while read max < "$limit"; [ "$max" != "2" ]; do sleep 0.1; done
		for i in 1 2 3 4 5; do sleep 60 & done
		wait`)
	spec.Resources = &manifest.Resources{Pids: 2}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.Require().Eventuallyf(func() bool {
		var out bytes.Buffer
		if err := s.client.Logs(s.ctx(), &out, name, client.WithTail(50)); err != nil {
			return false
		}

		return strings.Contains(out.String(), "fork")
	}, convergeTimeout, time.Second, "the workload never reported a refused fork")
}

// TestExecWorkloadPassesOnlyItsOwnEnvironment covers the environment an exec workload
// runs with, which is only what it named.
func (s *Suite) TestExecWorkloadPassesOnlyItsOwnEnvironment() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", `echo "[$GREETING]"; exit 0`)
	spec.Restart = &manifest.Restart{Policy: manifest.RestartOnFailure}
	spec.Env = map[string]string{"GREETING": "hello"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[hello]")
}

// TestExecJobIsRestartedWhenItFails covers an exec workload taking the same paced
// restart a container does, since the policy is the runtime's business either way.
func (s *Suite) TestExecJobIsRestartedWhenItFails() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", "exit 1")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartOnFailure}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	original := s.awaitInstance(name)

	s.Require().Eventuallyf(func() bool {
		current, err := s.client.Get(s.ctx(), name)
		if err != nil || len(current.Instances) == 0 {
			return false
		}

		return current.Instances[0].ID != original
	}, convergeTimeout, 500*time.Millisecond, "a failed exec job was never retried")
}

// TestLogsOfAReplacedContainerSurviveIt covers the reason log retention exists: a
// container workload that keeps failing, where the attempt now running has not failed
// yet and the one that did is the one worth reading.
//
// Before the driver kept a stopped container, this output was destroyed by the same pass
// that started the replacement, so `orca workload logs` reported the attempt which had
// yet to fail — exactly inverted from what the operator needs.
func (s *Suite) TestLogsOfAReplacedContainerSurviveIt() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// Each attempt announces the container it is running in and then fails, so the two
	// are told apart by their output rather than by timing. Docker sets the hostname to
	// the container's own identifier, which is what makes the marker unique per attempt
	// — a timestamp is not, since two attempts land in the same second. Something also
	// goes to stderr, which is what proves both streams survive retention.
	spec := s.containerSpec(name)
	spec.Container.Command = []string{"sh", "-c", `echo "attempt $(hostname)" ; echo on-stderr 1>&2 ; exit 1`}
	spec.Restart = &manifest.Restart{Policy: manifest.RestartAlways}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// The first attempt's output, read before anything replaces it.
	var first bytes.Buffer

	s.Require().Eventuallyf(func() bool {
		first.Reset()
		if err := s.client.Logs(s.ctx(), &first, name, client.WithTail(10)); err != nil {
			return false
		}

		return strings.Contains(first.String(), "attempt ")
	}, convergeTimeout, 500*time.Millisecond, "the first attempt never wrote anything")

	s.Contains(first.String(), "on-stderr", "the container's stderr was not captured")

	// Exactly which attempt was read, so what follows can assert that this one turns up
	// under --previous rather than merely that something did.
	replaced := strings.TrimSpace(first.String())

	// The retained output arrives once the reconciler has replaced that attempt. Keyed
	// on the marker that attempt wrote, so this cannot pass on its own output before
	// anything has replaced it.
	var previous bytes.Buffer

	s.Require().Eventuallyf(func() bool {
		previous.Reset()
		if err := s.client.Logs(s.ctx(), &previous, name, client.WithTail(10), client.WithPrevious()); err != nil {
			return false
		}

		return strings.TrimSpace(previous.String()) == replaced
	}, convergeTimeout, 500*time.Millisecond, "the replaced attempt's output was not kept")

	// The current output is a different attempt from the retained one, which is what
	// says the two are not the same container read twice.
	var current bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &current, name, client.WithTail(10)))

	s.NotEqual(previous.String(), current.String(), "--previous returned the attempt that is running now")

	// One retained container, however many times the workload has failed. Two would be
	// the disk leak retention is bounded to avoid, and the count includes the attempt
	// currently running.
	s.LessOrEqual(len(s.containers(name)), 2, "more than one stopped container was kept")
}

// TestFollowingAContainersOutput covers the other half of the log story: retention gives
// you the attempt that just failed, and following gives you the current one as it
// happens.
//
// Watching a workload start used to mean running the same command over and over. The
// tail of one is what makes this a test of the stream rather than of the tail: at most
// one line existed when the read opened, so a second one can only have arrived through
// the followed connection.
func (s *Suite) TestFollowingAContainersOutput() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Container.Command = []string{"sh", "-c", `i=0; while true; do i=$((i+1)); echo "line $i"; sleep 1; done`}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	ctx, cancel := context.WithCancel(s.ctx())
	s.T().Cleanup(cancel)

	var out syncBuffer

	done := make(chan error, 1)
	go func() {
		done <- s.client.Logs(ctx, &out, name, client.WithTail(1), client.WithFollow())
	}()

	s.Require().Eventuallyf(func() bool {
		return strings.Count(out.String(), "line ") > 1
	}, convergeTimeout, 500*time.Millisecond, "the followed output never arrived")

	// A caller pressing Ctrl-C is how most follows end, and it ends the read rather
	// than failing it. This is also what releases the server's end of the stream.
	cancel()

	select {
	case err = <-done:
		s.Require().NoError(err)
	case <-time.After(convergeTimeout):
		s.Fail("the follow outlived the caller that asked for it")
	}
}

// TestFollowingAProcessEndsWithIt covers the promise a follow makes about when it stops:
// the read ends when the instance does, so nothing has to be cancelled to get out of it.
//
// The exec runtime rather than the container one, because this is where it is not free.
// Docker closes a followed stream itself, where the exec driver polls a file and has to
// decide for itself that there is nothing more coming.
func (s *Suite) TestFollowingAProcessEndsWithIt() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", "echo started; sleep 5; echo finished")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	ctx, cancel := context.WithTimeout(s.ctx(), convergeTimeout)
	s.T().Cleanup(cancel)

	// Returns of its own accord. Nothing cancels this, so a follow that failed to
	// notice the process ending would hang here until the context gave up.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(ctx, &out, name, client.WithTail(10), client.WithFollow()))

	// Including what the process wrote on its way out. The driver reads its record
	// before the file, so the last lines of an instance that ends mid-poll survive.
	s.Contains(out.String(), "finished")
}

// TestLogsOfAReplacedProcessSurviveIt is the exec runtime's half of retention, which
// matters because the two runtimes have to mean the same thing by --previous.
//
// The audit that asked for retention believed exec was already unaffected. It was not:
// stopping an exec workload removed its output tree, so a replacement destroyed the
// output of the attempt it replaced exactly as a container did.
func (s *Suite) TestLogsOfAReplacedProcessSurviveIt() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// An exec workload has no container to name itself after, so the marker is the
	// process's own pid: unique per attempt, where a timestamp is not.
	spec := s.execSpec(name, "sh", "-c", `echo "attempt $$" ; echo on-stderr 1>&2 ; exit 1`)
	spec.Restart = &manifest.Restart{Policy: manifest.RestartAlways}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	var first bytes.Buffer

	s.Require().Eventuallyf(func() bool {
		first.Reset()
		if err := s.client.Logs(s.ctx(), &first, name, client.WithTail(10)); err != nil {
			return false
		}

		return strings.Contains(first.String(), "attempt ")
	}, convergeTimeout, 500*time.Millisecond, "the first attempt never wrote anything")

	// Both streams go to one file, so this is what proves the file is the one kept.
	s.Contains(first.String(), "on-stderr", "the process's stderr was not captured")

	replaced := strings.TrimSpace(first.String())

	var previous bytes.Buffer

	s.Require().Eventuallyf(func() bool {
		previous.Reset()
		if err := s.client.Logs(s.ctx(), &previous, name, client.WithTail(10), client.WithPrevious()); err != nil {
			return false
		}

		return strings.TrimSpace(previous.String()) == replaced
	}, convergeTimeout, 500*time.Millisecond, "the replaced attempt's output was not kept")

	var current bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &current, name, client.WithTail(10)))

	s.NotEqual(previous.String(), current.String(), "--previous returned the attempt that is running now")
}

// TestARetainedContainerDoesNotMakeAWorkloadFail covers what retention must not do: a
// kept container has failed, and counting it would report a workload that is running
// perfectly well as broken.
func (s *Suite) TestARetainedContainerDoesNotMakeAWorkloadFail() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// A workload that fails once and then serves. The specification changes rather than
	// the command deciding, so the first attempt is replaced for the ordinary reason and
	// the second one stays up.
	spec := s.containerSpec(name)
	spec.Container.Command = []string{"sh", "-c", "echo first-attempt; exit 1"}
	spec.Restart = &manifest.Restart{Policy: manifest.RestartAlways}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	// Now a specification that stays up, which replaces the failing attempt.
	serving := s.containerSpec(name)
	serving.Restart = &manifest.Restart{Policy: manifest.RestartAlways}

	_, _, err = s.client.Apply(s.ctx(), serving)
	s.Require().NoError(err)

	// Running, not failed. The kept container is still there and still reports the
	// failure it ended with, so this is the assertion that it stays out of the state the
	// workload reports.
	workload := s.awaitState(name, client.WorkloadStateRunning)

	for _, instance := range workload.Instances {
		s.Equal(client.InstanceStateRunning, instance.State,
			"a container kept for its output was reported as an instance")
	}
}

// TestDeletingAWorkloadRemovesWhatWasRetained covers the other half of retention: a
// workload nobody wants any more has nobody to read its output, and a container left
// behind is one the orphan sweep would find on every pass from then on.
func (s *Suite) TestDeletingAWorkloadRemovesWhatWasRetained() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Container.Command = []string{"sh", "-c", "echo working; exit 1"}
	spec.Restart = &manifest.Restart{Policy: manifest.RestartAlways}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// Waited for so that something has actually been retained by the time it is
	// deleted, or the test would pass against a workload that never kept anything.
	s.Require().Eventuallyf(func() bool {
		var out bytes.Buffer
		if err := s.client.Logs(s.ctx(), &out, name, client.WithTail(10), client.WithPrevious()); err != nil {
			return false
		}

		return strings.Contains(out.String(), "working")
	}, convergeTimeout, 500*time.Millisecond, "nothing was ever retained to delete")

	_, err = s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// Nothing left, including what was kept. Docker is asked directly rather than the
	// API, since the question is what is on the host once orca says the workload is gone.
	s.Require().Eventuallyf(func() bool {
		return len(s.containers(name)) == 0
	}, convergeTimeout, 500*time.Millisecond, "a retained container outlived the workload it belonged to")
}

// TestExecWorkloadAdoptedAfterServerRestart covers the claim that makes restarting the
// server different from restarting the workloads it runs.
func (s *Suite) TestExecWorkloadAdoptedAfterServerRestart() {
	name := s.workloadName()

	// The first server's data has to outlive it, since the record the driver leaves
	// behind is what the next server adopts the process from.
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	_, _, err := s.client.Apply(s.ctx(), s.execSpec(name, "sleep", "600"))
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
	original := s.instanceID(name)

	s.restart(withDataDirectory(directory))
	s.T().Cleanup(func() { s.cleanup(name) })

	workload := s.awaitState(name, client.WorkloadStateRunning)
	s.Require().Len(workload.Instances, 1)

	// The same process, not a replacement. A server that started a second one would be
	// running the workload twice, and the first would be left with no supervisor.
	s.Equal(original, workload.Instances[0].ID)

	// This suite runs the server in its own process, so a child is never orphaned by
	// the restart and adoption is exercised without Release being involved. Whether a
	// process survives the server exiting is verified separately, by the driver's own
	// tests.
}

// TestExecWorkloadIsStoppedOnDelete covers the process going away with its workload,
// which nothing else would clean up.
func (s *Suite) TestExecWorkloadIsStoppedOnDelete() {
	name := s.workloadName()

	_, _, err := s.client.Apply(s.ctx(), s.execSpec(name, "sleep", "600"))
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	pid, err := strconv.Atoi(s.instanceID(name))
	s.Require().NoError(err)

	_, err = s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// A released process has no parent to reap it, so a delete that only removed the
	// row would leave it running for the life of the host.
	s.Require().Eventuallyf(func() bool {
		return syscall.Kill(pid, 0) != nil
	}, convergeTimeout, 500*time.Millisecond, "the process outlived the workload it belonged to")
}

// TestExecWorkloadCannotReachTheDataDirectory covers the confinement every exec
// workload runs under, against a real server holding real state.
//
// An exec workload runs as the server's own uid, so nothing about file ownership keeps
// it out of the database or the encryption key beside it. This is the one place that is
// exercised against a server that actually has both: the unit tests confine against a
// file a test wrote, where this confines against the real thing.
func (s *Suite) TestExecWorkloadCannotReachTheDataDirectory() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// The key the server generated on startup, which opens every secret it holds.
	// Named by an identifier, so it is found rather than assumed.
	keyring := filepath.Join(s.directory, "keys")

	entries, err := os.ReadDir(keyring)
	s.Require().NoError(err, "this test needs a keyring the server's own user can read")
	s.Require().Len(entries, 1)

	key := filepath.Join(keyring, entries[0].Name())

	contents, err := os.ReadFile(key)
	s.Require().NoError(err, "this test needs a key file the server's own user can read")
	s.Require().NotEmpty(contents)

	// Reading the key and the database, both of which sit in the data directory the
	// server is using. Ending cleanly either way, so this is about what the command
	// could read rather than how it exited.
	spec := s.execSpec(name, "sh", "-c",
		"cat "+key+" 2>&1; cat "+filepath.Join(s.directory, "state.db")+" 2>&1; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartOnFailure}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(20)))

	// The kernel refused both, so the workload printed the refusal rather than the
	// contents.
	s.NotContains(out.String(), string(contents), "an exec workload read the secret encryption key")
	s.Contains(out.String(), "Permission denied")
}

// TestScheduledWorkloadRunsOnItsSchedule covers the schedule against a real daemon: a
// workload runs at the times its expression names, ends, and runs again.
//
// A minute is the finest the standard cron form allows, so this is the slowest scenario
// in the suite. It earns that by being the only place the whole chain is exercised —
// the expression, the last run the driver reports, and the occurrence derived from
// both.
func (s *Suite) TestScheduledWorkloadRunsOnItsSchedule() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", "echo ran; exit 0")
	spec.Schedule = &manifest.Schedule{Cron: "* * * * *"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// The first occurrence has to arrive before anything runs, since applying a
	// workload is not one of the times a schedule names.
	first := s.awaitInstance(name)

	// The workload ends and is left alone rather than restarted, because a run that
	// ended cleanly did what its occurrence asked of it.
	s.awaitState(name, client.WorkloadStateStopped)

	// Once it has run, orca reports when it runs again.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.False(workload.NextRun.IsZero(), "a workload that has run should report its next occurrence")

	// The next occurrence replaces the run that ended, which is a new instance rather
	// than the same one started again.
	s.Require().Eventuallyf(func() bool {
		current, err := s.client.Get(s.ctx(), name)
		if err != nil || len(current.Instances) == 0 {
			return false
		}

		return current.Instances[0].ID != first
	}, convergeTimeout, time.Second, "the workload never ran a second occurrence")
}

// TestApplyDuringTeardownIsRejected covers re-applying a workload that is still being
// torn down, which would otherwise race the removal.
func (s *Suite) TestApplyDuringTeardownIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80, From: 8184})

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// Delete returns as soon as the intent is recorded, so the workload is still on
	// its way out here.
	deleted, err := s.client.Delete(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().True(deleted.Deleting)

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.True(client.IsConflict(err), "expected a conflict error, got %v", err)
}

// TestDryRunWritesNothing covers the property the whole feature rests on: a dry run
// reports what an apply would do and changes nothing about the node.
//
// It runs against the live server rather than a mock because that is the only place
// the claim can be tested. Nothing was written is a statement about the database and
// the port allocations, not about which functions were called.
func (s *Suite) TestDryRunWritesNothing() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{To: 80})

	// A workload nothing holds, with a port nothing has allocated. The host port is
	// reported as not yet known rather than invented, and the hash is withheld for
	// the same reason.
	planned, err := s.client.DryRun(s.ctx(), spec)
	s.Require().NoError(err)
	s.True(planned.Created)
	s.False(planned.Replaced)
	s.Empty(planned.SpecHash)
	s.Equal([]string{"$.ports[0].from"}, planned.Unknown)
	s.Require().Len(planned.Spec.Ports, 1)
	s.Zero(planned.Spec.Ports[0].From)

	// Reporting on a workload did not create one.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	applied, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.Require().Len(applied.Ports, 1)

	allocated := applied.Ports[0].From
	instance := s.awaitInstance(name)

	// The same manifest against the workload it created. Nothing moved, so nothing
	// would be replaced, and the allocation it holds is reported rather than a
	// second one being taken.
	unchanged, err := s.client.DryRun(s.ctx(), spec)
	s.Require().NoError(err)
	s.False(unchanged.Created)
	s.False(unchanged.Replaced)
	s.NotEmpty(unchanged.SpecHash)
	s.Empty(unchanged.Unknown)
	s.Empty(unchanged.Changed)
	s.Require().Len(unchanged.Spec.Ports, 1)
	s.Equal(allocated, unchanged.Spec.Ports[0].From)

	// A changed manifest is reported as replacing the instance, and still does not.
	changed := s.containerSpec(name, manifest.Port{To: 80})
	changed.Env = map[string]string{"EXAMPLE": "CHANGED"}

	replaced, err := s.client.DryRun(s.ctx(), changed)
	s.Require().NoError(err)
	s.True(replaced.Replaced)
	s.NotEqual(unchanged.SpecHash, replaced.SpecHash)
	// What moved, named to the field rather than to the block holding it.
	s.Equal([]string{"$.env.EXAMPLE"}, replaced.Changed)
	// The host port the workload holds is settled and reported, so nothing about it
	// reads as a change the operator made.
	s.NotContains(replaced.Changed, "$.ports[0].from")

	// The workload is where it was: same version, same allocation, same container.
	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Equal(applied.Version, after.Version)
	s.Require().Len(after.Ports, 1)
	s.Equal(allocated, after.Ports[0].From)
	s.Equal(instance, s.instanceID(name))
}

// TestDryRunOfAWorkloadReadingAVariable covers a workload whose hash depends on
// something its manifest does not contain.
//
// Setting a variable rehashes every workload reading it, so by the time the write
// returns the stored hash already accounts for the new value. A dry run of the same
// manifest reports no replacement either side of the move, which is the honest
// answer: the redeploy is already recorded, and reporting it again would have an
// operator expect a second one.
func (s *Suite) TestDryRunOfAWorkloadReadingAVariable() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetVariable(s.ctx(), variable, "first", nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	before, err := s.client.DryRun(s.ctx(), spec)
	s.Require().NoError(err)
	s.False(before.Replaced)

	_, _, err = s.client.SetVariable(s.ctx(), variable, "second", nil)
	s.Require().NoError(err)

	after, err := s.client.DryRun(s.ctx(), spec)
	s.Require().NoError(err)
	s.False(after.Replaced)

	// The value reaches the hash, so the hash moved even though the manifest did
	// not. That is what the workload is being replaced for.
	s.NotEqual(before.SpecHash, after.SpecHash)

	// The reported specification carries the reference rather than the value, as
	// the stored one does.
	s.Equal("${var:"+variable+"}", after.Spec.Env["VALUE"])
}

// TestDryRunRefusesWhatAnApplyRefuses covers the reason a dry run is worth running at
// all: a reference that cannot be resolved is reported now rather than as a workload
// in a restart loop.
func (s *Suite) TestDryRunRefusesWhatAnApplyRefuses() {
	name := s.workloadName()

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:does-not-exist}"}

	_, err := s.client.DryRun(s.ctx(), spec)
	s.True(client.IsBadRequest(err), "expected a bad request error, got %v", err)

	// Nothing was stored on the way to refusing it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestMissingWorkload covers the not-found path on every endpoint that takes a name.
func (s *Suite) TestMissingWorkload() {
	_, err := s.client.Get(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	_, err = s.client.Delete(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	s.ErrorIs(s.client.Logs(s.ctx(), io.Discard, "does-not-exist", client.WithTail(10)), client.ErrWorkloadNotFound)
}

// TestVolumeSurvivesAnExecWorkloadBeingReplaced covers the thing volumes exist for.
//
// Stopping an exec workload removes the tree its working directory sits in, and the
// reconciler stops a workload before every replacement and every retry. So a volume
// that did not survive that would be no better than the working directory it replaces,
// which is what made this worth writing before volumes existed.
func (s *Suite) TestVolumeSurvivesAnExecWorkloadBeingReplaced() {
	name := s.workloadName()
	volume := s.volumeName()

	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.CreateVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)
	s.Require().NotEmpty(created.Path)

	// The command writes through the relative path, since the volume is placed inside
	// the directory the process runs in. It appends, so a second run leaves both lines.
	spec := s.execSpec(name, "sh", "-c", "echo ran >> var/lib/example/runs; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)
	s.Require().Equal("ran\n", s.volumeFile(created.Path, "runs"))

	// A changed specification replaces the instance, which stops the workload and
	// removes its working directory before starting the new one.
	spec.Env = map[string]string{"CHANGED": "yes"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// Two lines rather than one: the volume kept what the first run wrote, and the
	// replacement appended to it rather than starting from nothing.
	s.Require().Eventually(func() bool {
		return s.volumeFile(created.Path, "runs") == "ran\nran\n"
	}, convergeTimeout, 250*time.Millisecond, "the volume did not survive the workload being replaced")
}

// TestVolumeOutlivesTheWorkloadThatMountsIt covers the lifecycle that makes a volume a
// resource of its own: deleting a workload leaves what it stored.
func (s *Suite) TestVolumeOutlivesTheWorkloadThatMountsIt() {
	name := s.workloadName()
	volume := s.volumeName()

	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.CreateVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	spec := s.execSpec(name, "sh", "-c", "echo precious > var/lib/example/file; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// While the workload exists, the volume reports it as a holder and refuses to be
	// deleted.
	held, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Equal([]string{name}, held.UsedBy)

	s.ErrorIs(s.client.DeleteVolume(s.ctx(), volume), client.ErrVolumeInUse)

	_, err = s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// The workload is gone and its data is not.
	s.Equal("precious\n", s.volumeFile(created.Path, "file"))

	free, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Empty(free.UsedBy, "the deleted workload is still reported as mounting the volume")

	// Only deleting the volume removes the data, which is the whole point of it being
	// a separate thing to delete.
	s.Require().NoError(s.client.DeleteVolume(s.ctx(), volume))

	_, err = s.client.GetVolume(s.ctx(), volume)
	s.ErrorIs(err, client.ErrVolumeNotFound)
	s.Empty(s.volumeFile(created.Path, "file"))
}

// TestVolumeMountedIntoAContainer covers the other runtime, where the volume is a bind
// mount and the workload uses the path as written.
func (s *Suite) TestVolumeMountedIntoAContainer() {
	name := s.workloadName()
	volume := s.volumeName()

	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.CreateVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	// An absolute path, used as written: a container has a filesystem of its own, so
	// the daemon can put the volume where the manifest says.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", "echo from-a-container > /var/lib/example/file"}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)
	s.Equal("from-a-container\n", s.volumeFile(created.Path, "file"))
}

// TestWorkloadMountingAnUnknownVolumeIsRejected covers the apply being refused rather
// than the volume being created, so a mistyped name is reported.
func (s *Suite) TestWorkloadMountingAnUnknownVolumeIsRejected() {
	name := s.workloadName()

	spec := s.execSpec(name, "sh", "-c", "exit 0")
	spec.Volumes = []manifest.VolumeMount{{Name: s.volumeName(), To: "/var/lib/example"}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)

	// The volume is named, since an operator who mistyped one can act on that and not
	// on "something went wrong".
	s.Contains(err.Error(), s.volumeName())

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	// And no volume was conjured up to satisfy the mount.
	_, err = s.client.GetVolume(s.ctx(), s.volumeName())
	s.ErrorIs(err, client.ErrVolumeNotFound)
}

// TestMissingVolume covers the not-found path on every volume endpoint that takes a
// name.
func (s *Suite) TestMissingVolume() {
	_, err := s.client.GetVolume(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrVolumeNotFound)

	s.ErrorIs(s.client.DeleteVolume(s.ctx(), "does-not-exist"), client.ErrVolumeNotFound)
}

// TestWorkloadReadsASecret covers the whole point of a secret: the value reaches the
// workload, and nothing else.
func (s *Suite) TestWorkloadReadsASecret() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, created, err := s.client.SetSecret(s.ctx(), secret, []byte("hunter2"), nil)
	s.Require().NoError(err)
	s.True(created)
	s.NotEmpty(stored.Revision)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$DSN]"; exit 0`}
	spec.Env = map[string]string{"DSN": "postgres://app:${secret:" + secret + "}@localhost/app"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Reading it back out of the container is the only proof the value was resolved
	// on the path that actually starts work.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[postgres://app:hunter2@localhost/app]")

	// What was stored is the reference, because the API echoes the specification back
	// to anything that can reach the server.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().NotNil(workload.Spec.Env)
	s.Contains(workload.Spec.Env["DSN"], "${secret:"+secret+"}")
	s.NotContains(workload.Spec.Env["DSN"], "hunter2")

	// And the value is nowhere in the database, including pages the write-ahead log
	// has not checkpointed. This is the property the whole feature exists for.
	s.False(s.databaseHolds("hunter2"), "the plaintext reached the database")
}

// TestRotatingASecretRedeploysItsWorkload covers a changed value replacing the
// instances reading the old one, which is what the revision in the hash is for.
func (s *Suite) TestRotatingASecretRedeploysItsWorkload() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("first"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	// The secret now reports the workload reading it, which is what makes a rotation
	// know what to redeploy.
	held, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal([]string{name}, held.UsedBy)

	rotated, created, err := s.client.SetSecret(s.ctx(), secret, []byte("second"), nil)
	s.Require().NoError(err)
	s.False(created)
	s.NotEqual(held.Revision, rotated.Revision)

	// The instance is replaced without the specification having changed at all.
	s.awaitInstanceOtherThan(name, instance)

	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Greater(after.Version, before.Version)
	s.Equal(before.Spec.Env, after.Spec.Env)
}

// TestUnchangedSecretIsNotRedeployed covers the no-op write, so that a tool setting
// every secret on every run does not restart the fleet each time.
func (s *Suite) TestUnchangedSecretIsNotRedeployed() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	first, _, err := s.client.SetSecret(s.ctx(), secret, []byte("unchanged"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	again, created, err := s.client.SetSecret(s.ctx(), secret, []byte("unchanged"), nil)
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.Revision, again.Revision)
	s.Equal(first.UpdatedAt, again.UpdatedAt)

	// Nothing moved, so nothing is replaced. Watched over several reconciliation
	// passes rather than asserted at once, since a redeploy would take a moment to
	// appear and an immediate check would pass whether or not one was coming.
	//
	// A plain loop rather than Never, whose condition runs on a goroutine that
	// outlives the assertion: it would still be reading the suite's client while the
	// teardown replaced it.
	for range 10 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(workload.Instances, 1)
		s.Require().Equal(before.Version, workload.Version,
			"an unchanged secret bumped its workload's version")
		s.Require().Equal(instance, workload.Instances[0].ID,
			"an unchanged secret replaced its workload's instance")
	}
}

// TestRelabellingASecretDoesNotRedeployIt covers the constraint that makes labels
// safe to put on a secret: filing one is not rotating it.
//
// A secret's revision is mixed into the specification hash of every workload reading
// it, so moving the revision replaces instances across the node. That is right for a
// value that changed and absurd for a piece of bookkeeping.
func (s *Suite) TestRelabellingASecretDoesNotRedeployIt() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	first, _, err := s.client.SetSecret(s.ctx(), secret, []byte("unchanged"), map[string]string{"app": "web"})
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "web"}, first.Labels)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	// The same value under different labels. The labels land and the revision does
	// not move.
	relabelled, created, err := s.client.SetSecret(s.ctx(), secret,
		[]byte("unchanged"), map[string]string{"app": "api", "team": "platform"})
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.Revision, relabelled.Revision)
	s.Equal(map[string]string{"app": "api", "team": "platform"}, relabelled.Labels)

	// Labels replace rather than merge, so setting the value with none removes them.
	// The revision still holds.
	cleared, _, err := s.client.SetSecret(s.ctx(), secret, []byte("unchanged"), nil)
	s.Require().NoError(err)
	s.Equal(first.Revision, cleared.Revision)
	s.Empty(cleared.Labels)

	// And nothing reading the secret was replaced by any of it. Watched over several
	// reconciliation passes, since a redeploy would take a moment to appear and an
	// immediate check would pass whether or not one was coming.
	for range 10 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(workload.Instances, 1)
		s.Require().Equal(before.Version, workload.Version,
			"relabelling a secret bumped its workload's version")
		s.Require().Equal(instance, workload.Instances[0].ID,
			"relabelling a secret replaced its workload's instance")
	}
}

// TestVolumeLabelsSurviveAnUpdate covers the write path a volume gained for its
// labels, which is the only thing about a volume that can change.
func (s *Suite) TestVolumeLabelsSurviveAnUpdate() {
	volume := s.volumeName()
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.CreateVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    volume,
		Labels:  map[string]string{"app": "web"},
	})
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "web"}, created.Labels)

	updated, err := s.client.UpdateVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    volume,
		Labels:  map[string]string{"app": "api"},
	})
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "api"}, updated.Labels)

	// The identifier the data is stored under is untouched, which is what makes this
	// safe to run against a volume holding something.
	s.Equal(created.Path, updated.Path)

	read, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Equal(map[string]string{"app": "api"}, read.Labels)
}

// TestDeletingASecretInUseIsRefused covers the refusal naming the workloads, and what
// forcing it does to them.
func (s *Suite) TestDeletingASecretInUseIsRefused() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("held"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	err = s.client.DeleteSecret(s.ctx(), secret)
	s.Require().ErrorIs(err, client.ErrSecretInUse)

	// The workload is named, since an operator can act on that and not on "something
	// is using it".
	s.Contains(err.Error(), name)

	// It is still there, so the refusal was a refusal rather than a report.
	_, err = s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)

	s.Require().NoError(s.client.DeleteSecret(s.ctx(), secret, client.WithForceDelete()))

	_, err = s.client.GetSecret(s.ctx(), secret)
	s.ErrorIs(err, client.ErrSecretNotFound)

	// Re-creating it recovers the workload on its own, which is what the link
	// outliving the secret is for.
	_, created, err := s.client.SetSecret(s.ctx(), secret, []byte("restored"), nil)
	s.Require().NoError(err)
	s.True(created)

	s.awaitState(name, client.WorkloadStateRunning)
}

// TestWorkloadReadingAnUnknownSecretIsRejected covers the apply being refused, so a
// mistyped name is reported rather than stored as a workload that can never start.
func (s *Suite) TestWorkloadReadingAnUnknownSecretIsRejected() {
	name := s.workloadName()

	spec := s.execSpec(name, "sh", "-c", "exit 0")
	spec.Env = map[string]string{"VALUE": "${secret:" + s.secretName() + "}"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), s.secretName())

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestSecretSurvivesAServerRestart covers the key being read back from disk, since a
// server that sealed a value has to be able to open it again.
func (s *Suite) TestSecretSurvivesAServerRestart() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, _, err := s.client.SetSecret(s.ctx(), secret, []byte("across-restarts"), nil)
	s.Require().NoError(err)

	s.restart(withDataDirectory(directory))

	// The revision is unchanged, so the secret was read rather than replaced.
	after, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal(stored.Revision, after.Revision)

	// And the value still opens under the key the new server loaded, which only a
	// workload reading it can show.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[across-restarts]")
}

// TestMissingSecret covers the not-found path on every secret endpoint that takes a
// name.
func (s *Suite) TestMissingSecret() {
	_, err := s.client.GetSecret(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrSecretNotFound)

	s.ErrorIs(s.client.DeleteSecret(s.ctx(), "does-not-exist"), client.ErrSecretNotFound)
}

// TestWorkloadReadsAVariable covers the whole point of a variable: the value reaches
// the workload, and an operator can still see what it is.
func (s *Suite) TestWorkloadReadsAVariable() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	stored, created, err := s.client.SetVariable(s.ctx(), variable, "localhost", nil)
	s.Require().NoError(err)
	s.True(created)
	s.Equal("localhost", stored.Value)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$DSN]"; exit 0`}
	spec.Env = map[string]string{"DSN": "postgres://app@${var:" + variable + "}/app"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Reading it back out of the container is the only proof the value was resolved
	// on the path that actually starts work.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[postgres://app@localhost/app]")

	// What was stored is the reference. A variable's value is not a secret, but
	// resolving it into the stored specification would mean a change to the variable
	// no longer reaching the workload that reads it.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().NotNil(workload.Spec.Env)
	s.Contains(workload.Spec.Env["DSN"], "${var:"+variable+"}")
	s.NotContains(workload.Spec.Env["DSN"], "localhost")

	// And unlike a secret, the value is readable back. This is the difference the
	// whole feature exists for.
	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal("localhost", held.Value)
	s.Equal([]string{name}, held.UsedBy)
}

// TestWorkloadReadsBothKinds covers one value holding a secret and a variable, which
// is what the single expansion pass is for.
func (s *Suite) TestWorkloadReadsBothKinds() {
	name := s.workloadName()
	secret, variable := s.secretName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("hunter2"), nil)
	s.Require().NoError(err)

	_, _, err = s.client.SetVariable(s.ctx(), variable, "db.internal", nil)
	s.Require().NoError(err)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$DSN]"; exit 0`}
	spec.Env = map[string]string{
		"DSN": "postgres://app:${secret:" + secret + "}@${var:" + variable + "}/app",
	}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Both kinds resolved in one value. A pass that handled only one would have
	// refused the other rather than substituting it.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[postgres://app:hunter2@db.internal/app]")

	// The secret's value is still nowhere in the database, even though the variable
	// beside it is. Holding both in one value does not weaken the secret.
	s.False(s.databaseHolds("hunter2"), "the plaintext reached the database")
}

// TestChangingAVariableRedeploysItsWorkload covers a changed value replacing the
// instances reading the old one, which is what the value in the hash is for.
func (s *Suite) TestChangingAVariableRedeploysItsWorkload() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetVariable(s.ctx(), variable, "first", nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	// The variable now reports the workload reading it, which is what makes a change
	// know what to redeploy.
	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal([]string{name}, held.UsedBy)

	changed, created, err := s.client.SetVariable(s.ctx(), variable, "second", nil)
	s.Require().NoError(err)
	s.False(created)
	s.Equal("second", changed.Value)

	// The instance is replaced without the specification having changed at all.
	s.awaitInstanceOtherThan(name, instance)

	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Greater(after.Version, before.Version)
	s.Equal(before.Spec.Env, after.Spec.Env)
}

// TestUnchangedVariableIsNotRedeployed covers the no-op write, so that a tool setting
// every variable on every run does not restart the fleet each time.
func (s *Suite) TestUnchangedVariableIsNotRedeployed() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	first, _, err := s.client.SetVariable(s.ctx(), variable, "unchanged", nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(name, client.WorkloadStateRunning)
	instance := s.awaitInstance(name)

	again, created, err := s.client.SetVariable(s.ctx(), variable, "unchanged", nil)
	s.Require().NoError(err)
	s.False(created)
	s.Equal(first.UpdatedAt, again.UpdatedAt)

	// Nothing moved, so nothing is replaced. Watched over several reconciliation
	// passes rather than asserted at once, since a redeploy would take a moment to
	// appear and an immediate check would pass whether or not one was coming.
	//
	// A plain loop rather than Never, whose condition runs on a goroutine that
	// outlives the assertion: it would still be reading the suite's client while the
	// teardown replaced it.
	for range 10 {
		time.Sleep(time.Second)

		workload, err := s.client.Get(s.ctx(), name)
		s.Require().NoError(err)
		s.Require().Len(workload.Instances, 1)
		s.Require().Equal(before.Version, workload.Version,
			"an unchanged variable bumped its workload's version")
		s.Require().Equal(instance, workload.Instances[0].ID,
			"an unchanged variable replaced its workload's instance")
	}
}

// TestDeletingAVariableInUseIsRefused covers the refusal naming the workloads, and
// what forcing it does to them.
func (s *Suite) TestDeletingAVariableInUseIsRefused() {
	name, variable := s.workloadName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetVariable(s.ctx(), variable, "held", nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	err = s.client.DeleteVariable(s.ctx(), variable)
	s.Require().ErrorIs(err, client.ErrVariableInUse)
	s.Contains(err.Error(), name)

	// Still there, so the refusal was a refusal rather than a report.
	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal("held", held.Value)

	s.Require().NoError(s.client.DeleteVariable(s.ctx(), variable, client.WithForceDeleteVariable()))

	_, err = s.client.GetVariable(s.ctx(), variable)
	s.ErrorIs(err, client.ErrVariableNotFound)

	// Re-creating it recovers the workload, which had been left unable to start.
	_, _, err = s.client.SetVariable(s.ctx(), variable, "held", nil)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
}

// TestWorkloadReadingAnUnknownVariableIsRejected covers the apply being refused rather
// than the workload being stored and left unable to start.
func (s *Suite) TestWorkloadReadingAnUnknownVariableIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:e2e-var-does-not-exist}"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), "e2e-var-does-not-exist")

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestVariableSurvivesAServerRestart covers the value outliving the process, since a
// variable that had to be set again after every restart would be useless.
func (s *Suite) TestVariableSurvivesAServerRestart() {
	variable := s.variableName()
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	directory := s.directory

	stored, _, err := s.client.SetVariable(s.ctx(), variable, "persisted", nil)
	s.Require().NoError(err)

	s.restart(withDataDirectory(directory))

	held, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal("persisted", held.Value)
	s.Equal(stored.UpdatedAt, held.UpdatedAt)
}

// TestMissingVariable covers the not-found path on every variable endpoint that takes
// a name.
func (s *Suite) TestMissingVariable() {
	_, err := s.client.GetVariable(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrVariableNotFound)

	s.ErrorIs(s.client.DeleteVariable(s.ctx(), "does-not-exist"), client.ErrVariableNotFound)
}

// TestWorkloadMountsValues covers the whole point of mounting a secret or a variable:
// the value reaches the workload as a file it can read, and nothing about the value is
// stored.
// TestWorkloadReachesAnotherWorkload covers the whole point of referencing a
// workload: the address orca chose reaches the workload it names, from inside another
// container.
func (s *Suite) TestWorkloadReachesAnotherWorkload() {
	backend, consumer := s.workloadName()+"-backend", s.workloadName()+"-consumer"
	s.T().Cleanup(func() { s.cleanup(consumer) })
	s.T().Cleanup(func() { s.cleanup(backend) })

	served, _, err := s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)
	s.Require().Len(served.Ports, 1)
	s.Equal("http", served.Ports[0].Name)
	s.True(served.Ports[0].Dynamic)

	s.awaitState(backend, client.WorkloadStateRunning)

	spec := s.jobSpec(consumer, manifest.RestartNever, 0)
	spec.Container.Command = []string{
		"sh", "-c", `echo "[$ADDR]"; wget -q -O- "http://$ADDR" >/dev/null && echo REACHED`,
	}
	spec.Env = map[string]string{"ADDR": "${workload:" + backend + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(consumer, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, consumer, client.WithTail(10)))

	// The host port orca allocated, resolved into the consumer's environment on the
	// path that actually starts work.
	s.Contains(out.String(), ":"+strconv.Itoa(served.Ports[0].From)+"]")

	// And a request over it is answered, which is the only thing that proves a
	// container can reach a port published for another workload.
	s.Contains(out.String(), "REACHED")

	// What was stored is the reference. An address in the specification would be one
	// the workload keeps after orca has moved it.
	workload, err := s.client.Get(s.ctx(), consumer)
	s.Require().NoError(err)
	s.Require().NotNil(workload.Spec.Env)
	s.Contains(workload.Spec.Env["ADDR"], "${workload:"+backend+":http}")
}

// TestMovingAPortRedeploysItsConsumers covers the address reaching the specification
// hash. Without it the reference would read as automatic and quietly would not be.
func (s *Suite) TestMovingAPortRedeploysItsConsumers() {
	backend, consumer := s.workloadName()+"-backend", s.workloadName()+"-consumer"
	s.T().Cleanup(func() { s.cleanup(consumer) })
	s.T().Cleanup(func() { s.cleanup(backend) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80, From: 8280}))
	s.Require().NoError(err)
	s.awaitState(backend, client.WorkloadStateRunning)

	spec := s.containerSpec(consumer)
	spec.Env = map[string]string{"ADDR": "${workload:" + backend + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitState(consumer, client.WorkloadStateRunning)
	instance := s.awaitInstance(consumer)

	// The backend moves to another host port, which is a change to the consumer's
	// address without a word of the consumer's manifest changing.
	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80, From: 8281}))
	s.Require().NoError(err)

	s.awaitInstanceOtherThan(consumer, instance)

	after, err := s.client.Get(s.ctx(), consumer)
	s.Require().NoError(err)
	s.Greater(after.Version, before.Version)
	s.Equal(before.Spec.Env, after.Spec.Env)
}

// TestDeletingAReferencedWorkload covers the guard on the other end of a reference,
// since applying a workload that names one which does not exist is rejected.
func (s *Suite) TestDeletingAReferencedWorkload() {
	backend, consumer := s.workloadName()+"-backend", s.workloadName()+"-consumer"
	s.T().Cleanup(func() { s.cleanup(consumer) })
	s.T().Cleanup(func() { s.cleanup(backend) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(backend, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)

	spec := s.containerSpec(consumer)
	spec.Env = map[string]string{"ADDR": "${workload:" + backend + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	_, err = s.client.Delete(s.ctx(), backend)
	s.Require().ErrorIs(err, client.ErrWorkloadInUse)
	s.Contains(err.Error(), consumer)

	// Still there, and still running, since the refusal happened before anything was
	// marked.
	held, err := s.client.Get(s.ctx(), backend)
	s.Require().NoError(err)
	s.False(held.Deleting)

	// Forcing it through is the operator saying they know. The consumer keeps running
	// until it next starts, which is when the reference it can no longer resolve
	// matters.
	deleted, err := s.client.Delete(s.ctx(), backend, client.WithForceDeleteWorkload(), client.WithWait())
	s.Require().NoError(err)
	s.True(deleted.Deleting)
}

// TestWorkloadsMayReferenceEachOther covers a cycle, which looks alarming and is not:
// allocating a port does not consult a reference, so there is no fixpoint to solve.
func (s *Suite) TestWorkloadsMayReferenceEachOther() {
	first, second := s.workloadName()+"-first", s.workloadName()+"-second"
	s.T().Cleanup(func() { s.cleanup(second) })
	s.T().Cleanup(func() { s.cleanup(first) })

	// A reference has to name a workload that exists, so the cycle is reached by
	// applying one, then the other, then the first again.
	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(first, manifest.Port{Name: "http", To: 80}))
	s.Require().NoError(err)

	other := s.containerSpec(second, manifest.Port{Name: "http", To: 80})
	other.Env = map[string]string{"PEER": "${workload:" + first + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), other)
	s.Require().NoError(err)

	closing := s.containerSpec(first, manifest.Port{Name: "http", To: 80})
	closing.Env = map[string]string{"PEER": "${workload:" + second + ":http}"}

	_, _, err = s.client.Apply(s.ctx(), closing)
	s.Require().NoError(err)

	// Both resolve and both run. Neither is waiting on the other.
	s.awaitState(first, client.WorkloadStateRunning)
	s.awaitState(second, client.WorkloadStateRunning)
}

// TestReferencingAnUnknownWorkloadIsRejected covers the apply-time guard, which is
// what an unknown secret gets and for the same reason.
func (s *Suite) TestReferencingAnUnknownWorkloadIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"ADDR": "${workload:e2e-workload-does-not-exist}"}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), "e2e-workload-does-not-exist")

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

func (s *Suite) TestWorkloadMountsValues() {
	name, secret, variable := s.workloadName(), s.secretName(), s.variableName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte(`{"key":"hunter2"}`), nil)
	s.Require().NoError(err)

	_, _, err = s.client.SetVariable(s.ctx(), variable, `{"level":"debug"}`, nil)
	s.Require().NoError(err)

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `cat /var/secret.json /var/example.json; exit 0`}
	spec.Volumes = []manifest.VolumeMount{
		{Secret: secret, To: "/var/secret.json"},
		{Var: variable, To: "/var/example.json"},
	}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	// Reading the files from inside the container is the only proof they were written
	// and mounted where the manifest asked for.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), `{"key":"hunter2"}`)
	s.Contains(out.String(), `{"level":"debug"}`)

	// What was stored is the name of what is read. A path resolved into the stored
	// specification would put orca's own layout in the API and move the hash with
	// every version.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(workload.Spec.Volumes, 2)
	s.Equal(secret, workload.Spec.Volumes[0].Secret)
	s.Equal(variable, workload.Spec.Volumes[1].Var)

	// The secret's value is nowhere in the database, exactly as for one an environment
	// reads. Mounting one writes it to the filesystem, which is the documented cost of
	// asking for a file.
	s.False(s.databaseHolds("hunter2"), "the mounted secret's value reached the database")

	// Both report the workload reading them, which is what makes a change reach it.
	heldSecret, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal([]string{name}, heldSecret.UsedBy)

	heldVariable, err := s.client.GetVariable(s.ctx(), variable)
	s.Require().NoError(err)
	s.Equal([]string{name}, heldVariable.UsedBy)
}

// TestReadOnlyRootfsLeavesMountsUsable covers the interaction between a read-only
// root filesystem and what orca mounts: a volume and a mounted value are bind mounts
// with rules of their own, so the volume stays writable and the value stays readable
// at its 0444 mode while the image's own filesystem refuses writes.
func (s *Suite) TestReadOnlyRootfsLeavesMountsUsable() {
	name, secret, volume := s.workloadName(), s.secretName(), s.volumeName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("hunter2"), nil)
	s.Require().NoError(err)

	created, err := s.client.CreateVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	// The command proves all three properties at once: the mounted value is readable,
	// the volume accepts a write, and the root filesystem does not. The workload only
	// exits cleanly when the write to the rootfs failed.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.ReadOnly = true
	spec.Container.Command = []string{"sh", "-c",
		`cat /var/secret.txt && echo written > /var/lib/example/file && ! touch /rootfs-write`}
	spec.Volumes = []manifest.VolumeMount{
		{Secret: secret, To: "/var/secret.txt"},
		{Name: volume, To: "/var/lib/example"},
	}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "hunter2")

	s.Equal("written\n", s.volumeFile(created.Path, "file"))
}

// TestChangingAMountedValueRedeploysItsWorkload covers the default delivery mode: a
// mount naming no signal is replaced, exactly as a referenced secret is.
func (s *Suite) TestChangingAMountedValueRedeploysItsWorkload() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("first"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	applied, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)
	original := s.instanceID(name)

	_, created, err := s.client.SetSecret(s.ctx(), secret, []byte("second"), nil)
	s.Require().NoError(err)
	s.False(created)

	// A workload that reads its file once at startup has to be replaced to see a new
	// value, which is what a moved hash arranges.
	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)

		return err == nil && workload.Version > applied.Version
	}, convergeTimeout, 500*time.Millisecond, "rotating a mounted secret did not bump the workload's version")

	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)

		return err == nil && len(workload.Instances) == 1 &&
			workload.Instances[0].ID != original &&
			workload.Instances[0].State == client.InstanceStateRunning
	}, convergeTimeout, 500*time.Millisecond, "rotating a mounted secret did not replace the instance")

	// The replacement reads the new value, which is what the whole exercise was for.
	s.Equal("second", s.mountedFile(name, "/var/secret"))
}

// TestSignallingAMountedValueKeepsTheWorkload covers the other delivery mode, which is
// the reason a mount may name a signal: the file changes underneath a workload that
// keeps running.
func (s *Suite) TestSignallingAMountedValueKeepsTheWorkload() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("first"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{
		{Secret: secret, To: "/var/secret", Signal: manifest.SignalHUP},
	}

	applied, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	original := s.instanceID(name)
	s.Require().Equal("first", s.mountedFile(name, "/var/secret"))

	_, created, err := s.client.SetSecret(s.ctx(), secret, []byte("second"), nil)
	s.Require().NoError(err)
	s.False(created)

	// The file changes underneath the running container, which only works because it is
	// rewritten in place: a bind mount follows the inode, so a replaced file would
	// leave the container reading the old value forever.
	s.Require().Eventuallyf(func() bool {
		return s.mountedFile(name, "/var/secret") == "second"
	}, convergeTimeout, 500*time.Millisecond, "the mounted secret was never refreshed")

	// And the workload was never disturbed. This is what naming a signal asks for, and
	// what a moved hash would have prevented.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Equal(applied.Version, workload.Version, "signalling a mounted secret bumped the workload's version")
	s.Require().Len(workload.Instances, 1)
	s.Equal(original, workload.Instances[0].ID, "signalling a mounted secret replaced the instance")
	s.Equal(client.InstanceStateRunning, workload.Instances[0].State)
}

// TestDeletingAWorkloadRemovesItsMountedValues covers the plaintext leaving the disk,
// which is what bounds how long mounting a secret exposes it.
func (s *Suite) TestDeletingAWorkloadRemovesItsMountedValues() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("on-disk"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// The file exists while the workload does, which is the cost of mounting a value.
	s.Require().Equal("on-disk", s.mountedFile(name, "/var/secret"))
	s.Require().True(s.mountsHold("on-disk"), "the mounted secret was never written to the host")

	_, err = s.client.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// And no longer, which is what makes mounting a bounded exposure rather than a
	// permanent one.
	s.False(s.mountsHold("on-disk"), "the mounted secret outlived the workload")
}

// TestMountingAnUnknownValueIsRejected covers the apply being refused rather than the
// workload being stored and left unable to start.
func (s *Suite) TestMountingAnUnknownValueIsRejected() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Secret: "e2e-sec-does-not-exist", To: "/var/secret"}}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().Error(err)
	s.Contains(err.Error(), "e2e-sec-does-not-exist")

	// Nothing was stored, so the reconciler never sees it.
	_, err = s.client.Get(s.ctx(), name)
	s.ErrorIs(err, client.ErrWorkloadNotFound)
}

// TestExecWorkloadMountsAValue covers a mounted value reaching the other runtime, which
// places it by symlink rather than by bind mount and so reads it by the relative path.
func (s *Suite) TestExecWorkloadMountsAValue() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("exec-mounted"), nil)
	s.Require().NoError(err)

	// The relative path, as for a mounted volume: making the absolute one resolve there
	// would need the process confined to its own directory.
	spec := s.execSpec(name, "sh", "-c", `cat var/secret; exit 0`)
	spec.Restart = &manifest.Restart{Policy: manifest.RestartOnFailure}
	spec.Volumes = []manifest.VolumeMount{{Secret: secret, To: "/var/secret"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "exec-mounted")
}

// TestObservability covers the surface an operator points a monitor at: liveness,
// readiness, and the metrics scrape a Prometheus would take.
func (s *Suite) TestObservability() {
	s.Require().NoError(s.client.Health(s.ctx()))

	// Readiness needs a completed pass over every driver, so it is awaited
	// rather than asserted.
	s.Require().Eventually(func() bool {
		readiness, err := s.client.Ready(s.ctx())

		return err == nil && readiness.Ready
	}, convergeTimeout, 100*time.Millisecond, "server never reported ready")

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	var metrics bytes.Buffer
	s.Require().NoError(s.client.Metrics(s.ctx(), &metrics))

	// The pass counter proves orca's own instruments are on the scrape, and the
	// workload gauge proves per-workload measurement made it through a real
	// converge.
	s.Contains(metrics.String(), "orca_reconcile_passes_total")
	s.Contains(metrics.String(), "orca_workloads")
}

// TestDebugBundle proves every test leaves the server's spans and logs on disk,
// which is what a failed run is diagnosed from.
func (s *Suite) TestDebugBundle() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	// Spans and logs are batched and only flushed by the server shutting down,
	// so restarting is what makes the bundle readable mid-test — and proves a
	// restarting test accumulates both servers' output in one bundle.
	s.restart(withDataDirectory(s.directory))

	trace, err := os.ReadFile(filepath.Join(s.artifacts, "trace.json"))
	s.Require().NoError(err)
	s.Contains(string(trace), `"reconcile"`, "the pass's root span is in the bundle")

	logs, err := os.ReadFile(filepath.Join(s.artifacts, "logs.json"))
	s.Require().NoError(err)
	s.NotEmpty(logs, "the server's own log records are in the bundle")
}

// TestBackupRestoresANode proves the whole point of the backup command: an archive
// taken from a server that is running restores onto an empty data directory and the
// node comes back.
//
// The database runs in write-ahead logging mode, so most of what a busy node has
// committed can be in the log rather than in state.db. A backup that copied the
// database file alone would restore a node missing whatever was applied most
// recently, and it would do it silently.
func (s *Suite) TestBackupRestoresANode() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, _, err := s.client.SetSecret(s.ctx(), secret, []byte("survives-a-restore"), nil)
	s.Require().NoError(err)

	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	// Taken against the server that is still running, which is what makes this
	// different from stopping orca and copying files.
	var archive bytes.Buffer
	s.Require().NoError(s.client.Backup(s.ctx(), &archive, client.WithKeys()))

	restored := s.T().TempDir()
	s.restore(archive.Bytes(), restored)

	s.restart(withDataDirectory(restored))

	// The workload is in the restored database, and the server converged onto it
	// rather than treating the running container as an orphan.
	s.awaitState(name, client.WorkloadStateRunning)

	// The revision is unchanged, so the secret was restored rather than replaced.
	after, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal(stored.Revision, after.Revision)

	// And the value opens under the key that came out of the archive, which only a
	// workload reading it can show.
	// A name of its own. Every name helper answers per test, so reusing one would
	// replace the workload under test rather than add a second one beside it.
	reader := s.workloadName() + "-reader"
	s.T().Cleanup(func() { s.cleanup(reader) })

	spec := s.jobSpec(reader, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(reader, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, reader, client.WithTail(10)))
	s.Contains(out.String(), "[survives-a-restore]")
}

// TestRestoreBringsBackVolumeData covers the part of a restore no archive can carry
// and no server can perform, which is the part that fails silently when it is got
// wrong.
//
// Volume data is arbitrary user data and is not in a backup, so it is copied by hand.
// A volume is found by the identifier it was assigned rather than by its name, so
// creating one of the same name on the restored node would give a fresh identifier,
// an empty volume, and a row pointing at nothing. Nothing about that reads as an
// error: the workload starts and its storage is empty.
//
// The workload writing into the volume runs on the host rather than in a container,
// so the files it leaves belong to the user running the test and copying them needs
// no privileges.
func (s *Suite) TestRestoreBringsBackVolumeData() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, volume := s.workloadName(), s.volumeName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupVolume(volume) })

	created, err := s.client.CreateVolume(s.ctx(), manifest.Volume{Version: "v1", Name: volume})
	s.Require().NoError(err)

	writer := s.execSpec(name, "sh", "-c", "echo precious > var/lib/example/file; exit 0")
	writer.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	writer.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), writer)
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateCompleted)

	s.Require().Equal("precious\n", s.volumeFile(created.Path, "file"))

	var archive bytes.Buffer
	s.Require().NoError(s.client.Backup(s.ctx(), &archive, client.WithKeys()))

	restored := s.T().TempDir()
	report := s.restore(archive.Bytes(), restored)

	// The restore says what it could not do, naming the identifier and the directory
	// the data belongs in. Without that an operator has a row and no way to work out
	// which directory answers to it.
	s.Empty(report.MissingKeys)
	s.Equal([]restore.Volume{{
		ID:   filepath.Base(created.Path),
		Name: volume,
		Path: filepath.Join(restored, "volumes", filepath.Base(created.Path)),
	}}, report.MissingVolumes)

	s.Require().NoError(os.MkdirAll(filepath.Join(restored, "volumes"), 0o700))
	s.Require().NoError(os.CopyFS(report.MissingVolumes[0].Path, os.DirFS(created.Path)))

	s.restart(withDataDirectory(restored))

	// The row still names the same directory, which is what the copy relied on.
	after, err := s.client.GetVolume(s.ctx(), volume)
	s.Require().NoError(err)
	s.Equal(report.MissingVolumes[0].Path, after.Path)
	s.Equal("precious\n", s.volumeFile(after.Path, "file"))

	// And a workload mounting it by name reaches the data, which is the whole chain:
	// the name resolves to the identifier, the identifier to the directory, and the
	// directory holds what was backed up.
	// A name of its own. Every name helper answers per test, so reusing one would
	// replace the workload under test rather than add a second one beside it.
	reader := s.workloadName() + "-reader"
	s.T().Cleanup(func() { s.cleanup(reader) })

	spec := s.execSpec(reader, "sh", "-c", "cat var/lib/example/file; exit 0")
	spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	spec.Volumes = []manifest.VolumeMount{{Name: volume, To: "/var/lib/example"}}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.awaitState(reader, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, reader, client.WithTail(10)))
	s.Contains(out.String(), "precious")
}

// TestBackupLeavesTheKeyringOut proves the default keeps the database and the keys
// apart, which is what makes an archive safe to keep somewhere a key would not be.
func (s *Suite) TestBackupLeavesTheKeyringOut() {
	var archive bytes.Buffer
	s.Require().NoError(s.client.Backup(s.ctx(), &archive))

	reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	s.Require().NoError(err)

	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
	}

	s.Equal([]string{"state.db"}, names)
}

// TestRekeyKeepsSecretsReadable proves the point of the command: every secret is
// re-sealed under a new key and a workload reading one still gets its value.
//
// The workload is started before the rekey and read after it, so this covers the
// value surviving the rewrite rather than merely being set again.
func (s *Suite) TestRekeyKeepsSecretsReadable() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	stored, _, err := s.client.SetSecret(s.ctx(), secret, []byte("survives-a-rekey"), nil)
	s.Require().NoError(err)

	rekey, err := s.client.Rekey(s.ctx())
	s.Require().NoError(err)
	s.NotEmpty(rekey.KeyID)
	s.NotEqual(rekey.KeyID, rekey.PreviousKeyID)
	s.GreaterOrEqual(rekey.Secrets, 1)

	// The revision is unchanged. A rekey changes how a value is stored, not what it
	// is, so nothing reading it has any reason to be replaced.
	after, err := s.client.GetSecret(s.ctx(), secret)
	s.Require().NoError(err)
	s.Equal(stored.Revision, after.Revision)

	// And the value opens under the key the rekey produced, which only a workload
	// reading it can show.
	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[survives-a-rekey]")
}

// TestRekeyDoesNotRedeployWorkloads proves the guarantee an operator decides a
// maintenance window on. A rekey moves no revision, so no specification hash moves
// and the instances that were running keep running.
func (s *Suite) TestRekeyDoesNotRedeployWorkloads() {
	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("unchanged-by-a-rekey"), nil)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.awaitState(name, client.WorkloadStateRunning)

	before, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(before.Instances, 1)

	_, err = s.client.Rekey(s.ctx())
	s.Require().NoError(err)

	// A pass has to have run against the rekeyed database before this means anything,
	// or it would pass simply by being asked too early.
	s.awaitState(name, client.WorkloadStateRunning)

	after, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(after.Instances, 1)

	s.Equal(before.Version, after.Version, "the rekey moved the workload's version")
	s.Equal(before.Instances[0].SpecHash, after.Instances[0].SpecHash, "the rekey moved a specification hash")

	// The same instance, not a replacement that happens to hash the same.
	s.Equal(before.Instances[0].ID, after.Instances[0].ID, "the rekey replaced the running instance")
}

// TestRekeySurvivesAServerRestart proves the database is what names the current key.
// A restarted server has a keyring holding both the old key and the new one, and only
// the rows say which of them opens what.
func (s *Suite) TestRekeySurvivesAServerRestart() {
	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	name, secret := s.workloadName(), s.secretName()
	s.T().Cleanup(func() { s.cleanup(name) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })

	_, _, err := s.client.SetSecret(s.ctx(), secret, []byte("across-a-rekey"), nil)
	s.Require().NoError(err)

	rekey, err := s.client.Rekey(s.ctx())
	s.Require().NoError(err)

	// Both keys are on disk, so picking the newest file rather than reading the
	// database would be a coin toss the server must not be making.
	entries, err := os.ReadDir(filepath.Join(directory, "keys"))
	s.Require().NoError(err)
	s.Len(entries, 2)

	s.restart(withDataDirectory(directory))

	spec := s.jobSpec(name, manifest.RestartNever, 0)
	spec.Container.Command = []string{"sh", "-c", `echo "[$VALUE]"; exit 0`}
	spec.Env = map[string]string{"VALUE": "${secret:" + secret + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateCompleted)

	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(10)))
	s.Contains(out.String(), "[across-a-rekey]")

	s.NotEmpty(rekey.PreviousKeyID)
}
