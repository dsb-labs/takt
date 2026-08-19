package service

import (
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
)

// The Health type reports what orca established about a workload's health, and
// whether it checks the workload at all.
type Health struct {
	// Whether the workload declares a check orca performs.
	Checked bool
	// The most recent outcome, meaningful only when Checked.
	Result health.Result
}

// The Checker interface describes how the service reads the health of workloads.
//
// Registering the checks is the reconciler's job rather than this one's: a check has
// to be kept in step with what is actually running, and the reconciler is what runs
// continuously. Registering on read would mean a restarted server checked nothing
// until somebody happened to look.
type Checker interface {
	// Result should return the most recent outcome for a workload, reporting false
	// when it has no check registered.
	Result(workload string) (health.Result, bool)
}

// health returns what orca knows about a workload's health.
func (s *WorkloadService) health(workload string) Health {
	if s.checker == nil {
		return Health{}
	}

	result, checked := s.checker.Result(workload)

	return Health{Checked: checked, Result: result}
}

// healthState reports the instance state a workload's health implies, so that a
// workload which is running but not working converges rather than being left alone.
//
// A failing check makes an instance failed, which routes it into the same paced
// restart a crashed container takes — the reaction to "not working" is the same
// whether the process died or merely stopped answering. A check that has not yet
// passed makes the instance pending, which the reconciler treats as up: a workload
// still starting must not be replaced for not having answered yet.
func healthState(state driver.State, reported Health) driver.State {
	if !reported.Checked || state != driver.StateRunning {
		return state
	}

	switch reported.Result.Status {
	case health.StatusUnhealthy:
		return driver.StateFailed
	case health.StatusStarting:
		return driver.StatePending
	default:
		return state
	}
}
