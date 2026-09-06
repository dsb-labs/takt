// Package state derives what a workload is doing overall from the instances the
// driver reports for it.
//
// The derivation lives in its own package because two tiers perform it: the
// workload service reports a state to the API, and the reconciler counts states
// for its metrics. Owned here rather than taken from the wire format, because a
// state is something takt establishes rather than something a caller submits. The
// HTTP API maps it onto the state it publishes, as it does every other field.
package state

import (
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The Workload type names what a workload is doing overall, derived from the
// instances the driver reports and whether the workload is being torn down.
type Workload string

const (
	// Pending is a workload the reconciler has yet to start.
	Pending Workload = "pending"
	// Running is a workload with an instance up.
	Running Workload = "running"
	// Degraded is a workload with at least one instance up and at least one
	// failed. It serves traffic, and part of it does not work.
	Degraded Workload = "degraded"
	// Terminating is a workload being torn down, or one whose instance is on its
	// way out.
	Terminating Workload = "terminating"
	// Stopped is a workload whose instance ended cleanly and whose restart policy
	// will run it again.
	Stopped Workload = "stopped"
	// Completed is a workload whose run finished, which is the end a job is meant
	// to reach.
	Completed Workload = "completed"
	// Failed is a workload whose instance ended badly.
	Failed Workload = "failed"
	// Suspended is a workload that has been stopped and is intentionally not
	// running.
	Suspended Workload = "suspended"
)

// Workloads names every state a workload can read as, for a caller keying
// something by all of them.
var Workloads = []Workload{
	Completed,
	Degraded,
	Failed,
	Pending,
	Running,
	Stopped,
	Suspended,
	Terminating,
}

// Completion reports the state an ended instance reads as once its workload's
// restart policy has had its say.
//
// Only a clean exit the policy retires becomes completed. An instance that exited
// non-zero stays failed however the policy treats it, because how a workload ended and
// whether it runs again are separate facts: a job retired under "never" still has to
// say that it failed, or an operator reading it would see a success.
//
// An instance still running is untouched. The policy describes what happens when work
// ends, and this one has not ended.
func Completion(instance driver.Instance, restart *manifest.Restart) driver.State {
	if instance.State != driver.StateExited {
		return instance.State
	}

	// Attempts are not counted here. A workload that gave up has ended, and how it
	// ended is what this reports: giving up is the reconciler's decision about whether
	// to run it again.
	if restart.Policy.Restarts(instance.ExitCode) {
		return instance.State
	}

	return driver.StateCompleted
}

// Of derives a workload's overall state from its instances and whether it is
// being deleted or suspended.
//
// A workload marked for deletion is terminating whatever its instances are doing,
// because that is the only thing that will happen to it from here — reporting it as
// running while it is on its way out would invite a caller to wait for something
// that is never coming back.
//
// A suspended workload reads as suspended on the same reasoning: an instance still
// up is mid-stop, and nothing will run until the workload is started again. Neither
// stopped nor completed would be true — the server does not intend to fix it, and
// its restart policy did not ask for the end.
//
// Otherwise running wins, with one exception: a running instance beside a failed
// one reads as degraded, because the failure is a sibling's own doing rather than
// a consequence of anything routine — and running would mask it. A clean exit or
// a terminating predecessor beside a running instance stays running, since a
// restart and a rolling replacement are both routine. Then teardown in progress
// is reported ahead of how the departing instance ended, since the exit is a
// consequence of the teardown rather than news in its own right. Failure outranks
// a clean exit, and a workload with no instances at all is pending, because the
// reconciler has yet to start it.
func Of(instances []driver.Instance, deleting, suspended bool) Workload {
	if deleting {
		return Terminating
	}

	if suspended {
		return Suspended
	}

	if len(instances) == 0 {
		return Pending
	}

	var running, terminating, failed, exited, completed bool
	for _, instance := range instances {
		switch instance.State {
		case driver.StateRunning:
			running = true
		case driver.StateTerminating:
			terminating = true
		case driver.StateFailed:
			failed = true
		case driver.StateExited:
			exited = true
		case driver.StateCompleted:
			completed = true
		}
	}

	// Ranked so that nothing masks a problem. A workload with one completed instance
	// and one failed instance is failed: the completion is true but it is not the fact
	// an operator needs first.
	switch {
	case running && failed:
		return Degraded
	case running:
		return Running
	case terminating:
		return Terminating
	case failed:
		return Failed
	case exited:
		return Stopped
	case completed:
		return Completed
	default:
		return Pending
	}
}
