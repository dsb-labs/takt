// Package e2e provides end-to-end tests that exercise a real takt server against a
// real Docker daemon.
//
// These tests are the only place takt's layers are exercised together as an operator
// uses them: a manifest goes in through the client and containers come out on the
// daemon. They deliberately cover whole journeys rather than individual behaviours —
// the unit tests own the edge cases — and they are what catches the mistakes that
// only appear when the real runtime is involved, such as a container removal racing
// the shutdown it was meant to follow.
//
// The suite is not parallel, and cannot be. Every server shares one Docker daemon,
// and the reconciler stops any takt-labelled container that no workload asks for, so
// two servers running at once would tear down each other's work.
package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/suite"

	execdriver "github.com/dsb-labs/takt/internal/server/driver/exec"
	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

const (
	// The image the tests run. Small, quick to start, and long-running, so a
	// workload built from it stays up until takt stops it.
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

	// Refused before any test runs, rather than skipped. The driver takes the cgroup
	// it was started in for its delegated subtree, and a terminal's own scope
	// qualifies: run there, the suite moves the terminal's processes into the leaf
	// it makes and runs workloads beside them, and terminals have died that way.
	// The script sets this for the scope it makes, and nothing else should.
	if os.Getenv("TAKT_DELEGATED_SCOPE") == "" {
		fmt.Fprintln(os.Stderr, `this suite prepares the cgroup it is started in: run it through "make e2e" or scripts/delegated.sh`)
		os.Exit(1)
	}

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
// only the port inside the container and takt picks the host port that reaches it.
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

// TestWorkloadRestartedAfterItDies covers recovery from a workload dying, which takt
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
// down leaves behind: a container takt owns that no workload asks for.
func (s *Suite) TestOrphanedContainerIsStopped() {
	name := s.workloadName()

	// Removed here as well as by takt. The test asserts takt reaps it, so a failure
	// leaves it behind — and the name is derived from the test, so the container would
	// then collide with the next run of it. Docker reports that as an exit status
	// rather than as a message, which is a poor thing to debug from.
	s.T().Cleanup(func() { s.cleanup(name) })

	container := "takt-" + name + "-orphan"

	// Any output docker produces is captured, because the exit status alone says
	// nothing about why: a name conflict and a missing image look the same.
	create := exec.Command("docker", "run", "--detach",
		"--name", container,
		"--label", "takt.workload="+name,
		"--label", "takt.spec-hash=deadbeef",
		"--label", "takt.version=1",
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
	// clean exit is not a reason to stop, so takt brings it back.
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
// restart policy exists for: the runtime reports it gone, and takt must leave it gone.
func (s *Suite) TestCompletedJobIsNotRestarted() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.jobSpec(name, manifest.RestartOnFailure, 0))
	s.Require().NoError(err)

	workload := s.awaitState(name, client.WorkloadStateCompleted)
	s.Require().Len(workload.Instances, 1)
	s.Equal(client.InstanceStateCompleted, workload.Instances[0].State)

	// The instance that ran stays the instance that ran. Anything else means takt
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

// TestEventsRecordWhyAWorkloadLooksTheWayItDoes covers the read surface end to end.
// A workload that was applied and then started records both, so an operator reading
// the events sees the cause rather than only the state.
func (s *Suite) TestEventsRecordWhyAWorkloadLooksTheWayItDoes() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	_, _, err := s.client.Apply(s.ctx(), s.containerSpec(name))
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	events, err := s.client.Events(s.ctx(), name)
	s.Require().NoError(err)

	reasons := make(map[client.EventReason]client.Event, len(events))
	for _, recorded := range events {
		reasons[recorded.Reason] = recorded
	}

	s.Contains(reasons, client.EventApplied, "the apply that created the workload was not recorded")
	s.Require().Contains(reasons, client.EventInstanceStarted, "the instance the reconciler started was not recorded")

	// Rendered by the server from the reason and the data, rather than stored as a
	// sentence, so a client reads words without holding a table of them.
	started := reasons[client.EventInstanceStarted]
	s.Equal("Started instance 0", started.Message)
	s.Positive(started.Count)
	s.False(started.LastSeen.Before(started.FirstSeen))
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
	spec.Container.Image = "takt-e2e/does-not-exist:latest"
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
// that started the replacement, so `takt workload logs` reported the attempt which had
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

// TestServingTLS covers the server terminating TLS itself: the client trusts the
// self-signed pair through its certificate authority option, a workload goes in
// and comes out over the encrypted connection, and a followed log read still
// streams. The follow matters because a TLS listener negotiates HTTP/2, and a
// stream that buffers under it would pass every other request while breaking
// this one.
func (s *Suite) TestServingTLS() {
	s.restart(s.withTLS())

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

	// A second line can only have arrived through the followed connection, which
	// is what proves the stream flushes over TLS.
	s.Require().Eventuallyf(func() bool {
		return strings.Count(out.String(), "line ") > 1
	}, convergeTimeout, 500*time.Millisecond, "the followed output never arrived over tls")

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
	// API, since the question is what is on the host once takt says the workload is gone.
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

// TestContainerOutputIsCapped covers the logs block reaching the daemon, which is the
// one place the cap on a container's output is applied.
func (s *Suite) TestContainerOutputIsCapped() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Logs = &manifest.Logs{MaxSize: "1m", MaxFiles: 2}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// Asked of docker rather than of takt, since what the manifest said and what the
	// daemon will rotate on are the two things this test has to see agree.
	ids := s.containers(name)
	s.Require().NotEmpty(ids)

	out, err := exec.Command("docker", "inspect", "--format", `{{index .HostConfig.LogConfig.Config "max-size"}} {{index .HostConfig.LogConfig.Config "max-file"}}`, ids[0]).Output()
	s.Require().NoError(err)
	s.Equal("1m 2", strings.TrimSpace(string(out)))
}

// TestExecWorkloadOutputIsRotated covers the cap on an exec workload's output against a
// real server, whose watch of the runtime is what rotates it.
func (s *Suite) TestExecWorkloadOutputIsRotated() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", `i=0; while :; do i=$((i+1)); echo "line $i"; sleep 0.005; done`)
	spec.Logs = &manifest.Logs{MaxSize: "1k", MaxFiles: 2}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitState(name, client.WorkloadStateRunning)

	// The rotated file sits beside the output in the workload's directory, which is
	// named for an identifier the test does not know. The record beside it names the
	// workload, so the directory is found through the record rather than guessed.
	s.Require().Eventuallyf(func() bool {
		matches, _ := filepath.Glob(filepath.Join(s.directory, "exec", "workloads", "*", "0", "*", "output.log.1"))
		for _, match := range matches {
			var recorded struct {
				Workload string `json:"workload"`
			}

			record := strings.Replace(filepath.Dir(match), filepath.Join("exec", "workloads"), filepath.Join("exec", "state"), 1)

			data, err := os.ReadFile(filepath.Join(record, "state.json"))
			if err == nil && json.Unmarshal(data, &recorded) == nil && recorded.Workload == name {
				return true
			}
		}

		return false
	}, convergeTimeout, 500*time.Millisecond, "the output of %q was never rotated", name)

	// A read reaches into the rotated file, so a tail just after a rotation is not
	// a handful of lines. The rotated file holds the cap's worth, which is over a
	// hundred of these lines.
	var out bytes.Buffer
	s.Require().NoError(s.client.Logs(s.ctx(), &out, name, client.WithTail(1000)))
	s.Greater(strings.Count(out.String(), "\n"), 100)
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

	// Once it has run, takt reports when it runs again.
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

	_, _, err := s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "first"})
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"VALUE": "${var:" + variable + "}"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstance(name)

	before, err := s.client.DryRun(s.ctx(), spec)
	s.Require().NoError(err)
	s.False(before.Replaced)

	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "second"})
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

// TestListQueryOnLabels covers filtering the volume, secret and variable lists by
// a query into their labels, with the syntax the workload list already accepts.
func (s *Suite) TestListQueryOnLabels() {
	marker := s.volumeName()
	web, api := marker+"-web", marker+"-api"
	secret, variable := s.secretName(), s.variableName()
	s.T().Cleanup(func() { s.cleanupVolume(web) })
	s.T().Cleanup(func() { s.cleanupVolume(api) })
	s.T().Cleanup(func() { s.cleanupSecret(secret) })
	s.T().Cleanup(func() { s.cleanupVariable(variable) })

	// The suite shares one server, so the labels carry this test's unique names
	// rather than values another test might also use.
	_, err := s.client.ApplyVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    web,
		Labels:  map[string]string{"suite": marker, "role": "web"},
	})
	s.Require().NoError(err)

	_, err = s.client.ApplyVolume(s.ctx(), manifest.Volume{
		Version: "v1",
		Name:    api,
		Labels:  map[string]string{"suite": marker, "role": "api"},
	})
	s.Require().NoError(err)

	// A label is reached under $.labels, exactly as it is on a workload.
	volumes, err := s.client.ListVolumes(s.ctx(), "$.labels.suite="+marker)
	s.Require().NoError(err)
	s.Require().Len(volumes, 2)
	s.Equal(api, volumes[0].Name)
	s.Equal(web, volumes[1].Name)

	// Queries are combined, so adding one narrows rather than widens.
	narrowed, err := s.client.ListVolumes(s.ctx(), "$.labels.suite="+marker, "$.labels.role=web")
	s.Require().NoError(err)
	s.Require().Len(narrowed, 1)
	s.Equal(web, narrowed[0].Name)

	// Nothing matching is an empty result, not an error.
	none, err := s.client.ListVolumes(s.ctx(), "$.labels.suite="+marker, "$.labels.role=nope")
	s.Require().NoError(err)
	s.Empty(none)

	// A malformed query is the caller's mistake and has to be reported as such.
	_, err = s.client.ListVolumes(s.ctx(), "$.labels.role")
	s.True(client.IsBadRequest(err), "expected a bad request error, got %v", err)

	// The same query narrows the secret list.
	_, _, err = s.client.SetSecret(s.ctx(), manifest.Secret{Name: secret, Value: []byte("held"), Labels: map[string]string{"suite": marker}})
	s.Require().NoError(err)

	secrets, err := s.client.ListSecrets(s.ctx(), "$.labels.suite="+marker)
	s.Require().NoError(err)
	s.Require().Len(secrets, 1)
	s.Equal(secret, secrets[0].Name)

	_, err = s.client.ListSecrets(s.ctx(), "$.labels.suite")
	s.True(client.IsBadRequest(err), "expected a bad request error, got %v", err)

	// And the variable list.
	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: variable, Value: "held", Labels: map[string]string{"suite": marker}})
	s.Require().NoError(err)

	variables, err := s.client.ListVariables(s.ctx(), "$.labels.suite="+marker)
	s.Require().NoError(err)
	s.Require().Len(variables, 1)
	s.Equal(variable, variables[0].Name)

	_, err = s.client.ListVariables(s.ctx(), "$.labels.suite")
	s.True(client.IsBadRequest(err), "expected a bad request error, got %v", err)
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

	// The pass counter proves takt's own instruments are on the scrape, and the
	// workload gauge proves per-workload measurement made it through a real
	// converge.
	s.Contains(metrics.String(), "takt_reconcile_passes_total")
	s.Contains(metrics.String(), "takt_workloads")
}

// TestNode covers the read an operator takes of the machine itself: what the
// server is, and what the host has.
func (s *Suite) TestNode() {
	node, err := s.client.GetNode(s.ctx())
	s.Require().NoError(err)

	hostname, err := os.Hostname()
	s.Require().NoError(err)
	s.Equal(hostname, node.Hostname)
	s.Positive(node.CPUs)
	s.NotEmpty(node.Kernel)

	// The version is whatever the build carries, which for a server started
	// inside the test process is not a release, so only its presence is
	// asserted.
	s.NotEmpty(node.Version)

	// The instants are read from the kernel and recorded at startup rather
	// than derived from the clock, so their order is the check.
	s.False(node.StartedAt.IsZero())
	s.True(node.BootedAt.Before(node.StartedAt))

	s.Positive(node.Memory.Total)
	s.LessOrEqual(node.Memory.Used, node.Memory.Total)

	// The directories are the configured ones, reported whether or not the
	// first volume has created the volumes directory yet.
	s.Equal(s.directory, node.Disks.Data.Path)
	s.Equal(filepath.Join(s.directory, "volumes"), node.Disks.Volumes.Path)
	s.Positive(node.Disks.Data.Total)
	s.LessOrEqual(node.Disks.Data.Free, node.Disks.Data.Total)

	// A workload naming limits adds them to what the node has promised once
	// it runs, and one naming none is counted rather than summed.
	limited := s.workloadName() + "-limited"
	unlimited := s.workloadName() + "-unlimited"
	s.T().Cleanup(func() { s.cleanup(limited) })
	s.T().Cleanup(func() { s.cleanup(unlimited) })

	spec := s.containerSpec(limited)
	spec.Resources = &manifest.Resources{Memory: "64m", CPU: 0.25}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	_, _, err = s.client.Apply(s.ctx(), s.containerSpec(unlimited))
	s.Require().NoError(err)
	s.awaitState(limited, client.WorkloadStateRunning)
	s.awaitState(unlimited, client.WorkloadStateRunning)

	after, err := s.client.GetNode(s.ctx())
	s.Require().NoError(err)
	s.Equal(node.Allocated.Memory+64<<20, after.Allocated.Memory)
	s.InDelta(node.Allocated.CPU+0.25, after.Allocated.CPU, 0.001)
	s.Equal(node.Allocated.UnlimitedMemory+1, after.Allocated.UnlimitedMemory)
	s.Equal(node.Allocated.UnlimitedCPU+1, after.Allocated.UnlimitedCPU)
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

// TestWorkloadRunsMultipleInstances covers count as an operator uses it: apply a
// count, get that many instances at addresses of their own, and scale down to fewer.
func (s *Suite) TestWorkloadRunsMultipleInstances() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name, manifest.Port{Name: "http", To: 80})
	spec.Count = 3

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstances(name, 3)

	// Three instances publish the same container port at three host ports, and
	// every one of them really answers.
	workload, err := s.client.Get(s.ctx(), name)
	s.Require().NoError(err)
	s.Require().Len(workload.Ports, 3)

	hosts := make(map[int]struct{}, 3)
	for _, port := range workload.Ports {
		hosts[port.From] = struct{}{}
		s.awaitListening("127.0.0.1:" + strconv.Itoa(port.From))
	}

	s.Len(hosts, 3, "each instance holds a host port of its own")

	// Scaling down removes the high instance, its container, and its port rows.
	spec.Count = 2
	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	instances := s.awaitInstances(name, 2)
	s.NotContains(instances, 2, "the removed index is gone")

	s.Require().Eventuallyf(func() bool {
		workload, err = s.client.Get(s.ctx(), name)

		return err == nil && len(workload.Ports) == 2
	}, convergeTimeout, 500*time.Millisecond, "the removed instance's ports were never released")
}

// TestOneInstanceReplacedAlone covers the point of per-instance convergence: one
// instance dying is one replacement, and its siblings are untouched.
func (s *Suite) TestOneInstanceReplacedAlone() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Count = 2

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	before := s.awaitInstances(name, 2)

	s.Require().NoError(exec.Command("docker", "kill", before[1].ID).Run())

	// The killed instance is replaced and the survivor is exactly the container it
	// was. A workload-wide replacement would have moved both identifiers.
	s.Require().Eventuallyf(func() bool {
		workload, err := s.client.Get(s.ctx(), name)
		if err != nil {
			return false
		}

		after := make(map[int]client.Instance, len(workload.Instances))
		for _, instance := range workload.Instances {
			after[instance.Index] = instance
		}

		return len(after) == 2 &&
			after[0].ID == before[0].ID &&
			after[1].ID != before[1].ID &&
			after[1].State == client.InstanceStateRunning
	}, convergeTimeout, 500*time.Millisecond, "the killed instance was never replaced on its own")
}

// TestLogsSelectAnInstance covers the instance selector and the rules around it.
func (s *Suite) TestLogsSelectAnInstance() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.containerSpec(name)
	spec.Count = 2

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitInstances(name, 2)

	// One instance's output is one nginx announcing itself.
	var logs strings.Builder
	s.Require().NoError(s.client.Logs(s.ctx(), &logs, name, client.WithTail(50), client.WithInstance(1)))
	s.Contains(logs.String(), "nginx")

	// A follow with no selection has no one stream to read.
	err = s.client.Logs(s.ctx(), &logs, name, client.WithFollow())
	s.Require().Error(err)
	s.Contains(err.Error(), "select an instance")

	// An index the count does not include is refused rather than answered with
	// nothing.
	err = s.client.Logs(s.ctx(), &logs, name, client.WithInstance(5))
	s.Require().Error(err)
	s.Contains(err.Error(), "no instance")
}

// TestExecWorkloadRunsMultipleInstances covers count on the exec runtime, whose
// instances are processes with records and directories of their own.
func (s *Suite) TestExecWorkloadRunsMultipleInstances() {
	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	spec := s.execSpec(name, "sh", "-c", "sleep 60")
	spec.Count = 2

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	instances := s.awaitInstances(name, 2)
	s.NotEqual(instances[0].ID, instances[1].ID, "two instances are two processes")
}

// TestServiceReportsBackends covers the service resource end to end: a service
// TestPrometheusDiscovery covers the discovery endpoint end to end: a workload
// opts in by label, and the endpoint answers in the shape http_sd_configs
// reads, with a target per instance at the port the labels selected.
func (s *Suite) TestPrometheusDiscovery() {
	// The helper derives one name from the test, and this test needs three
	// workloads that survive each other's applies.
	labelled := s.workloadName() + "-full"
	bare := s.workloadName() + "-bare"
	silent := s.workloadName() + "-silent"

	s.T().Cleanup(func() {
		s.cleanup(labelled)
		s.cleanup(bare)
		s.cleanup(silent)
	})

	// Two ports, so the selection has something to choose, and two instances,
	// so the group carries a target per instance. The labels the endpoint
	// consumes select the port, and the rest of the namespace passes through.
	spec := s.containerSpec(labelled, manifest.Port{Name: "http", To: 80}, manifest.Port{Name: "metrics", To: 9100})
	spec.Count = 2
	spec.Labels = map[string]string{
		"prometheus.scrape": "true",
		"prometheus.port":   "metrics",
		"prometheus.path":   "/",
	}

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// One port and only the opt-in, so everything else is defaulted.
	spec = s.containerSpec(bare, manifest.Port{To: 9100})
	spec.Labels = map[string]string{"prometheus.scrape": "true"}

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// No labels, so the fleet having other workloads changes nothing.
	spec = s.containerSpec(silent, manifest.Port{To: 80})

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	// Discovery describes desired state, so the targets exist as soon as the
	// specifications are stored and their ports are allocated — nothing here
	// waits for an instance to run.
	resp, err := http.Get(s.address + "/api/v1/system/prometheus-sd")
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var groups []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&groups))

	found := make(map[string]int)
	for i, group := range groups {
		found[group.Labels["takt_workload"]] = i
	}

	s.NotContains(found, silent, "a workload that did not opt in was discovered")

	// The labelled workload: a target per instance, at the metrics port the
	// label selected, with the namespace passed through prefix-stripped.
	s.Require().Contains(found, labelled)
	group := groups[found[labelled]]
	s.Equal(labelled, group.Labels["job"])
	s.Equal("/", group.Labels["__metrics_path__"])
	s.NotContains(group.Labels, "scrape", "a consumed label leaked into the series")

	stored, err := s.client.Get(s.ctx(), labelled)
	s.Require().NoError(err)

	// The host half of a target is the configured workload address, which the
	// harness owns — the ports are what this test can hold the endpoint to.
	expected := make([]string, 0, 2)
	for _, port := range stored.Ports {
		if port.Name == "metrics" {
			expected = append(expected, strconv.Itoa(port.From))
		}
	}
	s.Require().Len(expected, 2)

	got := make([]string, 0, len(group.Targets))
	for _, target := range group.Targets {
		_, p, err := net.SplitHostPort(target)
		s.Require().NoError(err)
		got = append(got, p)
	}
	s.ElementsMatch(expected, got)

	// The bare workload: the sole port selected without a label, the job
	// defaulted to the workload's name.
	s.Require().Contains(found, bare)
	s.Equal(bare, groups[found[bare]].Labels["job"])
	s.Len(groups[found[bare]].Targets, 1)
}

// testPolicy is the document the auth tests apply: one principal per role,
// and a group the fake identity provider asserts.
var testPolicy = manifest.Policy{
	Version: "v1",
	OIDC:    &manifest.PolicyOIDC{PrincipalClaim: "email", GroupsClaim: "groups"},
	Grants: []manifest.PolicyGrant{
		{Principals: []string{"scraper"}, Role: manifest.RoleViewer},
		{Principals: []string{"operator@example.com"}, Role: manifest.RoleOperator},
		{Principals: []string{"group:infra"}, Role: manifest.RoleAdmin},
	},
}

// TestAuthDisabled proves that a server without an [auth] block behaves as it
// always has: anonymous requests hold the whole API, and whoami says so.
func (s *Suite) TestAuthDisabled() {
	identity, err := s.client.WhoAmI(s.ctx())
	s.Require().NoError(err)

	s.False(identity.Enabled)
	s.Equal("anonymous", identity.Principal)
	s.Equal("admin", identity.Role)

	// Anonymous writes still work: the network boundary is the whole check.
	name := s.variableName()
	defer s.cleanupVariable(name)

	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: name, Value: "value"})
	s.Require().NoError(err)
}

// TestAuthLifecycle walks the whole enablement journey: init once, apply the
// first policy with the recovery token, mint per-principal tokens, and prove
// each role holds exactly what the policy grants until revocation cuts it
// off.
func (s *Suite) TestAuthLifecycle() {
	s.restart(withAuth())

	// The probes carry no security requirement: a supervisor must reach them
	// without credentials or it cannot manage the process.
	s.Require().NoError(s.client.Health(s.ctx()))

	// Everything else is refused until a credential exists.
	_, err := s.client.List(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected 401, got %v", err)

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	_, err = s.client.InitACL(s.ctx())
	s.Require().ErrorIs(err, client.ErrACLInitialized)

	// The recovery token sits above policy, which is what lets it apply the
	// first document to a server that grants nothing to anyone yet.
	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	_, viewerToken, err := admin.CreateToken(s.ctx(), "scraper")
	s.Require().NoError(err)

	_, operatorToken, err := admin.CreateToken(s.ctx(), "operator@example.com")
	s.Require().NoError(err)

	viewer := s.clientWithToken(viewerToken)
	operator := s.clientWithToken(operatorToken)

	// The viewer reads everything, including the endpoints prometheus
	// consumes, and mutates nothing.
	_, err = viewer.List(s.ctx())
	s.Require().NoError(err)
	s.Require().NoError(viewer.Metrics(s.ctx(), io.Discard))

	name := s.variableName()
	defer s.cleanupVariable(name)

	_, _, err = viewer.SetVariable(s.ctx(), manifest.Variable{Name: name, Value: "value"})
	s.Require().True(client.IsForbidden(err), "expected 403, got %v", err)

	_, err = viewer.ListTokens(s.ctx())
	s.Require().True(client.IsForbidden(err), "expected 403, got %v", err)

	identity, err := viewer.WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.True(identity.Enabled)
	s.Equal("scraper", identity.Principal)
	s.Equal("viewer", identity.Role)

	// The operator drives resource lifecycle.
	_, _, err = operator.SetVariable(s.ctx(), manifest.Variable{Name: name, Value: "value"})
	s.Require().NoError(err)

	// Revocation is immediate: the token list names the viewer's credential,
	// and deleting it refuses the very next request.
	tokens, err := admin.ListTokens(s.ctx())
	s.Require().NoError(err)

	var viewerID string
	for _, token := range tokens {
		if token.Principal == "scraper" {
			viewerID = token.ID
		}
	}
	s.Require().NotEmpty(viewerID)

	s.Require().NoError(admin.DeleteToken(s.ctx(), viewerID))

	_, err = viewer.List(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected 401 after revocation, got %v", err)

	// Logout is self-revocation, so the operator can retire its own token.
	s.Require().NoError(operator.Logout(s.ctx()))

	_, err = operator.WhoAmI(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected 401 after logout, got %v", err)

	// The recovery token's revocation path is deliberately host-level.
	s.Require().ErrorIs(admin.Logout(s.ctx()), client.ErrRecoveryLogout)
}

// TestWorkloadTokenMountAuthenticates covers the file form of
// workload identity: the manifest names a principal, the server mints and
// mounts a credential as the instance starts, and the policy alone decides
// what that principal may do — a granted one holds its role, an ungranted one
// authenticates and holds nothing.
func (s *Suite) TestWorkloadTokenMountAuthenticates() {
	s.restart(withAuth())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{
		{Token: "scraper", To: "/var/run/takt/token", Signal: manifest.SignalHUP},
		// A principal the policy grants nothing. Nothing has to exist before a
		// manifest names one: identity is asserted, authority is granted.
		{Token: "nobody", To: "/var/run/takt/other"},
	}

	_, _, err = admin.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitRunningAs(admin, name)

	// The file holds the credential and nothing else, and it authenticates as
	// the principal the manifest named.
	credential := s.mountedFile(name, "/var/run/takt/token")
	s.Require().NotEmpty(credential, "the token was never mounted")

	scraper := s.clientWithToken(credential)

	identity, err := scraper.WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("scraper", identity.Principal)
	s.Equal("viewer", identity.Role)

	_, err = scraper.List(s.ctx())
	s.Require().NoError(err)

	// The ungranted principal authenticates and holds no role, exactly the
	// state a static token for one has. The policy stays the one description
	// of who may do what.
	other := s.clientWithToken(s.mountedFile(name, "/var/run/takt/other"))

	identity, err = other.WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("nobody", identity.Principal)
	s.Empty(identity.Role)

	_, err = other.List(s.ctx())
	s.Require().True(client.IsForbidden(err), "expected 403 for an ungranted principal, got %v", err)

	// Both credentials are auditable: the token list names them with the
	// workload source, so an operator can tell a projected credential from a
	// static one.
	tokens, err := admin.ListTokens(s.ctx())
	s.Require().NoError(err)

	minted := make(map[string]string, 2)
	for _, token := range tokens {
		if token.Source == "workload" {
			minted[token.Principal] = token.ID
		}
	}
	s.Contains(minted, "scraper")
	s.Contains(minted, "nobody")
}

// TestWorkloadEnvTokenRotatesWithTheInstance covers the env form: the
// credential is fixed at process start, so it lives for the instance and a
// replacement both mints a fresh one and revokes its predecessor.
func (s *Suite) TestWorkloadEnvTokenRotatesWithTheInstance() {
	s.restart(withAuth())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Env = map[string]string{"TAKT_TOKEN": "${token:scraper}"}

	_, _, err = admin.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitRunningAs(admin, name)

	credential := s.containerEnv(name, "TAKT_TOKEN")
	s.Require().NotEmpty(credential, "the token never reached the environment")

	identity, err := s.clientWithToken(credential).WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("scraper", identity.Principal)

	original := s.containers(name)
	s.Require().NotEmpty(original)

	_, err = admin.Restart(s.ctx(), name)
	s.Require().NoError(err)

	// A replaced instance gets a fresh credential. This is the whole rotation
	// story for the env form: an environment cannot change under a running
	// process, so rotation happens by replacement.
	var replacement string
	s.Require().Eventuallyf(func() bool {
		containers := s.containers(name)
		if len(containers) == 0 || containers[0] == original[0] {
			return false
		}

		replacement = s.containerEnv(name, "TAKT_TOKEN")

		return replacement != "" && replacement != credential
	}, convergeTimeout, 500*time.Millisecond, "the replacement never held a fresh token")

	_, err = s.clientWithToken(replacement).WhoAmI(s.ctx())
	s.Require().NoError(err)

	// And the predecessor died with its instance: the fresh mint replaced it,
	// so the token list never accumulates.
	s.Require().Eventuallyf(func() bool {
		_, err = s.clientWithToken(credential).WhoAmI(s.ctx())

		return client.IsUnauthorized(err)
	}, convergeTimeout, 500*time.Millisecond, "the replaced instance's token still authenticates")
}

// TestDeletingAWorkloadRevokesItsTokens covers the credential's life ending
// with the workload's, which is the wart the hand-managed static token had:
// nothing revoked it, and the token list accumulated credentials for
// workloads that were gone.
func (s *Suite) TestDeletingAWorkloadRevokesItsTokens() {
	s.restart(withAuth())

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	spec := s.containerSpec(name)
	spec.Volumes = []manifest.VolumeMount{{Token: "scraper", To: "/var/run/takt/token"}}

	_, _, err = admin.Apply(s.ctx(), spec)
	s.Require().NoError(err)

	s.awaitRunningAs(admin, name)

	credential := s.mountedFile(name, "/var/run/takt/token")
	s.Require().NotEmpty(credential, "the token was never mounted")

	_, err = s.clientWithToken(credential).WhoAmI(s.ctx())
	s.Require().NoError(err)

	_, err = admin.Delete(s.ctx(), name, client.WithWait())
	s.Require().NoError(err)

	// Revocation is immediate once the teardown lands: the very next request
	// presenting the credential finds nothing to hash to.
	s.Require().Eventuallyf(func() bool {
		_, err = s.clientWithToken(credential).WhoAmI(s.ctx())

		return client.IsUnauthorized(err)
	}, convergeTimeout, 500*time.Millisecond, "the deleted workload's token still authenticates")

	tokens, err := admin.ListTokens(s.ctx())
	s.Require().NoError(err)

	for _, token := range tokens {
		s.NotEqual("workload", token.Source, "a workload-minted token outlived its workload")
	}
}

// TestAuthPolicyConflict proves that a stale conditional apply is refused
// rather than silently clobbering a concurrent one.
func (s *Suite) TestAuthPolicyConflict() {
	s.restart(withAuth())

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	current, err := admin.GetPolicy(s.ctx())
	s.Require().NoError(err)

	// The first apply consumes the tag the second one still holds.
	_, err = admin.ApplyPolicy(s.ctx(), testPolicy, client.WithIfMatch(current.ETag))
	s.Require().NoError(err)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy, client.WithIfMatch(current.ETag))
	s.Require().ErrorIs(err, client.ErrPolicyChanged)

	// The one-step form reads the fresh tag itself, which is the re-run the
	// error asks for.
	applied, err := admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)
	s.Equal(testPolicy, applied.Spec)
}

// TestAuthReset proves the lockout recovery: the reset file removes the
// recovery token at startup, init works again, and everything else survives.
func (s *Suite) TestAuthReset() {
	directory := s.T().TempDir()
	s.restart(withAuth(), withDataDirectory(directory))

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	admin := s.clientWithToken(recovery)

	_, err = admin.ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	_, clientToken, err := admin.CreateToken(s.ctx(), "scraper")
	s.Require().NoError(err)

	// The operator lost the recovery token. Writing the reset file into the
	// data directory and restarting is the whole procedure.
	s.Require().NoError(os.WriteFile(filepath.Join(directory, "acl.reset"), nil, 0o600))
	s.restart(withAuth(), withDataDirectory(directory))

	// The old recovery token is gone, init works exactly once again, and the
	// client tokens and policy survived.
	_, err = s.clientWithToken(recovery).ListTokens(s.ctx())
	s.Require().True(client.IsUnauthorized(err), "expected the old recovery token to be revoked, got %v", err)

	_, err = s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	identity, err := s.clientWithToken(clientToken).WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("scraper", identity.Principal)
	s.Equal("viewer", identity.Role)
}

// TestAuthOIDC proves the whole exchange: an authorization code becomes an
// identity token signed by the issuer, which becomes a short-lived client
// token whose principal and groups come from the claims the policy maps.
func (s *Suite) TestAuthOIDC() {
	issuer, sign, answer := s.fakeIssuer()

	s.restart(withOIDC(issuer, "takt"))

	recovery, err := s.client.InitACL(s.ctx())
	s.Require().NoError(err)

	_, err = s.clientWithToken(recovery).ApplyPolicy(s.ctx(), testPolicy)
	s.Require().NoError(err)

	// The CLI's flow discovers the issuer from the server rather than from
	// flags, so the discovery endpoint has to answer anonymously.
	discovered, err := s.client.GetOIDC(s.ctx())
	s.Require().NoError(err)
	s.Equal(issuer, discovered.Issuer)
	s.Equal("takt", discovered.ClientID)

	// The CLI's flow ends at the authorization code: the server performs the
	// exchange, because the exchange is what needs the client secret. The
	// infra group carries admin through the policy's group grant, so the
	// login proves the groups claim travelled from the identity token.
	answer("infra-code", sign(map[string]any{
		"email":  "david@example.com",
		"groups": []string{"infra"},
	}))

	login, err := s.client.LoginCode(s.ctx(), "infra-code", "any-verifier", "http://127.0.0.1:8250/oidc/callback")
	s.Require().NoError(err)
	s.Equal("david@example.com", login.Principal)
	s.False(login.ExpiresAt.IsZero())

	identity, err := s.clientWithToken(login.Credential).WhoAmI(s.ctx())
	s.Require().NoError(err)
	s.Equal("david@example.com", identity.Principal)
	s.Equal("admin", identity.Role)
	s.Contains(identity.Groups, "infra")

	// An identity signed by somebody else is refused, even when the issuer
	// hands it over in exchange for a code.
	_, wrongSign, _ := s.fakeIssuer()

	answer("forged-code", wrongSign(map[string]any{"email": "forger@example.com"}))

	_, err = s.client.LoginCode(s.ctx(), "forged-code", "any-verifier", "http://127.0.0.1:8250/oidc/callback")
	s.Require().True(client.IsUnauthorized(err), "expected 401 for a foreign signature, got %v", err)

	// A code the issuer never handed out is refused by the issuer, and the
	// refusal reaches the caller as an invalid credential.
	_, err = s.client.LoginCode(s.ctx(), "unknown-code", "any-verifier", "http://127.0.0.1:8250/oidc/callback")
	s.Require().True(client.IsUnauthorized(err), "expected 401 for an unknown code, got %v", err)

	// A redirect that is not loopback would make the server an exchange
	// oracle for codes obtained some other way, so it is refused.
	_, err = s.client.LoginCode(s.ctx(), "infra-code", "any-verifier", "https://evil.example.com/callback")
	s.Require().True(client.IsBadRequest(err), "expected 400 for a foreign redirect, got %v", err)
}

// fakeIssuer runs an OIDC issuer for the test: a discovery document, a JWKS,
// a signer that mints identity tokens the way the real one would, and a
// token endpoint that answers each authorization code with the identity the
// test registered for it.
func (s *Suite) fakeIssuer() (string, func(claims map[string]any) string, func(code, idToken string)) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	s.Require().NoError(err)

	mux := http.NewServeMux()
	issuer := httptest.NewServer(mux)
	s.T().Cleanup(issuer.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer.URL,
			"authorization_endpoint":                issuer.URL + "/authorize",
			"token_endpoint":                        issuer.URL + "/token",
			"jwks_uri":                              issuer.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})

	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}},
		})
	})

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, &jose.SignerOptions{
		ExtraHeaders: map[jose.HeaderKey]any{"kid": "test"},
	})
	s.Require().NoError(err)

	sign := func(claims map[string]any) string {
		now := time.Now()

		token := map[string]any{
			"iss": issuer.URL,
			"aud": "takt",
			"sub": fmt.Sprintf("subject-%d", now.UnixNano()),
			"iat": now.Unix(),
			"exp": now.Add(time.Hour).Unix(),
		}
		for name, value := range claims {
			token[name] = value
		}

		payload, err := json.Marshal(token)
		s.Require().NoError(err)

		signed, err := signer.Sign(payload)
		s.Require().NoError(err)

		serialized, err := signed.CompactSerialize()
		s.Require().NoError(err)

		return serialized
	}

	// The token endpoint stands in for the exchange the server performs on
	// the CLI's behalf. It answers a registered code with the identity the
	// test chose for it, and refuses any other the way a real issuer would.
	var answers sync.Map

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		idToken, ok := answers.Load(r.FormValue("code"))
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access",
			"token_type":   "bearer",
			"id_token":     idToken,
		})
	})

	answer := func(code, idToken string) {
		answers.Store(code, idToken)
	}

	return issuer.URL, sign, answer
}
