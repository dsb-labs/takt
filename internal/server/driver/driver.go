// Package driver defines the vocabulary shared between the orca server and the
// runtimes it can run workloads on.
//
// A driver is responsible for turning a workload's specification into running
// work, for reporting what it currently has running, and for stopping it again.
// Drivers own their own bookkeeping: the server persists desired state only and
// asks the driver what is actually running, so a driver is free to record its
// instances however its runtime allows — container labels, unit properties, a
// pidfile — without that choice reaching the database or the API.
package driver

import (
	"time"
)

// The State type describes the state of a single instance as reported by its driver.
type State string

const (
	// StatePending indicates the instance has been created but is not yet running.
	StatePending State = "pending"
	// StateRunning indicates the instance is running.
	StateRunning State = "running"
	// StateTerminating indicates the instance is being torn down and will shortly
	// be gone.
	//
	// This is a state orca's own actions produce: replacing an outdated instance
	// and deleting a workload both stop a container before removing it, and a
	// runtime need not complete either step before reporting. It is deliberately
	// distinct from a failure, because nothing has gone wrong, and from an exit,
	// because there is nothing left to restart.
	StateTerminating State = "terminating"
	// StateExited indicates the instance ran to completion and exited cleanly.
	StateExited State = "exited"
	// StateFailed indicates the instance exited with a non-zero status, or could
	// not be started at all.
	StateFailed State = "failed"
)

type (
	// The Instance type describes one unit of work a driver is running on behalf of
	// a workload. It is observed state: every field reflects what the driver found
	// when asked, never what the server wishes were true.
	Instance struct {
		// The driver's opaque handle for this instance, such as a container ID.
		// Meaningful only to the driver that produced it.
		ID string
		// The name of the workload the instance belongs to.
		Workload string
		// The hash of the specification the instance was started from. When this
		// differs from the workload's desired hash, the instance is stale and
		// will be replaced.
		SpecHash string
		// The version of the specification the instance was started from.
		Version int
		// The instance's current state.
		State State
		// The exit code, set once the instance has exited.
		ExitCode int
		// The time the instance last started, if it has started at all.
		StartedAt time.Time
	}

	// The Event type reports that a driver's view of an instance has changed, so
	// that the server can reconcile sooner than its next scheduled pass.
	//
	// An event carries only the affected workload: it is a hint that something
	// moved, not a description of what. The reconciler responds by observing the
	// driver afresh, which keeps it correct even if events are coalesced, delayed,
	// or dropped entirely.
	Event struct {
		// The name of the workload whose state changed.
		Workload string
	}
)
