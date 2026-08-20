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
	"io"
	"os/exec"
	"strings"
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
	s.Require().NoError(s.client.Logs(s.ctx(), &logs, name, 50))
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
	spec.Restart = ""

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

// TestMissingWorkload covers the not-found path on every endpoint that takes a name.
func (s *Suite) TestMissingWorkload() {
	_, err := s.client.Get(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	_, err = s.client.Delete(s.ctx(), "does-not-exist")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	s.ErrorIs(s.client.Logs(s.ctx(), io.Discard, "does-not-exist", 10), client.ErrWorkloadNotFound)
}
