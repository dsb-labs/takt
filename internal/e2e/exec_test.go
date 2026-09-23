package e2e_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

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
