package service

import (
	"encoding/json"
	"fmt"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// The Health type reports what orca established about a workload's health, and
// whether it checks the workload at all.
type Health struct {
	// Whether the workload declares a check orca performs.
	Checked bool
	// The most recent outcome, meaningful only when Checked.
	Result health.Result
}

// The Checker interface describes how the service registers and reads the health of
// workloads.
type Checker interface {
	// Set should register the check for a workload, replacing any it already had.
	Set(workload string, check health.Check)
	// Forget should drop the check for a workload that no longer exists.
	Forget(workload string)
	// Result should return the most recent outcome for a workload, reporting false
	// when it has no check registered.
	Result(workload string) (health.Result, bool)
}

// registerCheck tells the checker how to probe a workload, or forgets it when the
// workload declares no check.
//
// The address comes from the host port orca allocated, which is why this belongs to
// the service rather than to the checker: the checker knows how to probe an address,
// and the service is what knows a workload's address.
func (s *WorkloadService) registerCheck(row database.Workload, ports []database.Port) {
	if s.checker == nil {
		return
	}

	check, ok, err := healthCheck(row, ports)
	switch {
	case err != nil:
		// The specification was validated before it was stored, so a check that
		// cannot be resolved now means the two have diverged rather than that the
		// operator made a mistake.
		s.logger.With("workload", row.Name, "error", err).Error("failed to resolve health check")
		return
	case !ok:
		s.checker.Forget(row.Name)
		return
	}

	s.checker.Set(row.Name, check)
}

// healthCheck resolves a stored workload's health check into something probeable,
// reporting false when the workload declares none.
func healthCheck(row database.Workload, ports []database.Port) (health.Check, bool, error) {
	var spec api.WorkloadSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return health.Check{}, false, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	resolved := manifest.NewSpec(spec)
	if resolved.Health == nil {
		return health.Check{}, false, nil
	}

	host, err := healthPort(*resolved.Health, ports)
	if err != nil {
		return health.Check{}, false, err
	}

	return health.Check{
		// Probing the loopback address rather than the published interface keeps the
		// check to traffic that never leaves the host.
		Address:     fmt.Sprintf("127.0.0.1:%d", host),
		HTTP:        resolved.Health.HTTP,
		Interval:    resolved.Health.Interval,
		Timeout:     resolved.Health.Timeout,
		Retries:     resolved.Health.Retries,
		StartPeriod: resolved.Health.StartPeriod,
	}, true, nil
}

// healthPort finds the host port that reaches the port the check names.
func healthPort(check manifest.Health, ports []database.Port) (int, error) {
	if len(ports) == 0 {
		return 0, fmt.Errorf("workload publishes no ports to check")
	}

	// Validation requires the port to be named when several are published, so an
	// unnamed one can only mean the single port the workload has.
	if check.Port == 0 {
		return ports[0].Host, nil
	}

	for _, port := range ports {
		if port.Container == check.Port {
			return port.Host, nil
		}
	}

	return 0, fmt.Errorf("port %d is not published by the workload", check.Port)
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
