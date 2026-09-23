package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

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
