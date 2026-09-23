package service

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/internal/server/state"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Workload type is the service's view of a workload: the desired state that
	// was submitted, together with what the driver reports is running for it.
	Workload struct {
		// The name that identifies the workload.
		Name string
		// Incremented every time the workload's specification changes.
		Version int
		// Which runtime the specification names.
		Runtime manifest.Runtime
		// The specification that was submitted.
		Spec manifest.Spec
		// Arbitrary key-value pairs attached to the workload.
		Labels map[string]string
		// The instances the driver is currently running for the workload.
		Instances []Instance
		// The port mappings the server settled on, including any it allocated.
		Ports []ResolvedPort
		// The workload's overall state, derived from its instances and whether it is
		// being deleted or suspended.
		State state.Workload
		// Whether the workload has been marked for deletion and is being torn down.
		Deleting bool
		// Whether the workload has been stopped and is intentionally not running.
		Suspended bool
		// The time the workload was first applied.
		CreatedAt time.Time
		// The time the workload's specification last changed, or a suspended
		// workload was last resumed.
		UpdatedAt time.Time
		// When the workload next runs, for one that names a schedule. Zero for a workload
		// that runs continuously, and for a scheduled one that has not run yet.
		NextRun time.Time
	}

	// The Instance type is the service's view of one instance: what the driver
	// observed, together with what takt itself established about it.
	//
	// The observation is embedded rather than copied field by field, because it is
	// the driver's answer and restating it here would be a second place for it to
	// drift. What takt establishes sits beside it: whether the instance passes the
	// check takt performs is not something a runtime reports, and a driver that had
	// to carry it would be answering a question it was never asked.
	Instance struct {
		driver.Instance
		// What takt established about whether the instance is working.
		Health Health
		// What the instance is consuming, against the limits its specification
		// asked for. Zero for an instance nothing can be read for, which an
		// instance that is not running is.
		Usage Usage
	}

	// The Health type reports what takt established about a workload's health,
	// and whether it checks the workload at all.
	Health struct {
		// Whether the workload declares a check takt performs.
		Checked bool
		// The most recent outcome, meaningful only when Checked.
		Result health.Result
	}

	// The ResolvedPort type describes a port mapping as it was actually applied,
	// carrying the host port the server settled on. This is what a caller uses to
	// reach the workload.
	ResolvedPort struct {
		// The index of the workload instance the port reaches.
		Instance int
		// What the specification called this port. Empty for one it did not name.
		Name string
		// The port the workload listens on inside its runtime.
		To int
		// The host port that reaches it.
		From int
		// The transport protocol the port is published on.
		Protocol manifest.Protocol
		// Whether the host port was allocated by the server rather than pinned by
		// the specification.
		Dynamic bool
	}
)

// How long a read will wait on the driver before reporting desired state without
// observed state. A caller asking what exists should not be held up indefinitely by a
// runtime that has stopped answering.
const observeTimeout = 10 * time.Second

func (s *WorkloadService) hydrate(ctx context.Context, row database.Workload) (Workload, error) {
	ports, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to read workload ports: %w", err)
	}

	instances := s.observeWorkload(ctx, row)

	return newWorkload(row, instances, ports, s.healths(row.Name, instances))
}

// observeWorkload asks each driver what it is running for one workload.
//
// Reading a single workload used to observe every workload on the host and keep one
// entry, so the cost of reading one grew with the number running. A load test
// measured a single read at half a millisecond against one workload and fifty-four
// against a hundred and sixty, for the same request — and this is the path every
// apply, delete, stop, start and restart returns through, not only a get.
//
// Failures are handled as they are for a full observation: reported, not returned.
// The reasoning there applies unchanged.
func (s *WorkloadService) observeWorkload(ctx context.Context, row database.Workload) []driver.Instance {
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	var instances []driver.Instance

	for _, runtime := range s.drivers {
		observed, err := runtime.ObserveWorkload(ctx, row.ID, row.Name)
		if err != nil {
			s.logger.With("error", err, "runtime", runtime.Name(), "workload", row.Name).
				Error("failed to observe driver instances")

			continue
		}

		instances = append(instances, observed...)
	}

	return instances
}

// observe groups the driver's instances by workload name.
//
// A driver that cannot be reached is deliberately not an error: the desired state
// is still worth reporting, and a read of it shouldn't fail because the runtime is
// briefly unavailable. The failure is logged and callers see workloads with no
// instances, which reads as pending.
//
// The call is bounded so that a wedged daemon makes a read of desired state slower
// rather than hanging it: a request that never returns is worse than one that
// reports what it does know.
func (s *WorkloadService) observe(ctx context.Context) map[string][]driver.Instance {
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	var instances []driver.Instance
	for _, runtime := range s.drivers {
		observed, err := runtime.Observe(ctx)
		if err != nil {
			// Reported rather than returned: desired state is still worth reading, and
			// a read should not fail because one runtime is briefly unavailable.
			s.logger.With("error", err, "runtime", runtime.Name()).Error("failed to observe driver instances")

			continue
		}

		instances = append(instances, observed...)
	}

	byWorkload := make(map[string][]driver.Instance, len(instances))
	for _, instance := range instances {
		byWorkload[instance.Workload] = append(byWorkload[instance.Workload], instance)
	}

	return byWorkload
}

// healths returns what takt knows about each instance's health, keyed by the
// instance's index. Only the observed instances are asked after, since a result can
// only exist for an instance that runs.
func (s *WorkloadService) healths(workload string, instances []driver.Instance) map[int]Health {
	if s.checker == nil {
		return nil
	}

	healths := make(map[int]Health, len(instances))

	for _, instance := range instances {
		if _, ok := healths[instance.Index]; ok {
			continue
		}

		result, checked := s.checker.Result(workload, instance.Index)
		healths[instance.Index] = Health{Checked: checked, Result: result}
	}

	return healths
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

func newWorkload(row database.Workload, instances []driver.Instance, ports []database.Port, healths map[int]Health) (Workload, error) {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return Workload{}, err
	}

	deleting := !row.DeletedAt.IsZero()
	suspended := !row.SuspendedAt.IsZero()
	policy := spec.Restart

	// An instance a driver keeps only so that its output can still be read is left out
	// of what the workload reports. It has ended and nothing will restart it, so
	// reporting it would have a workload that is running perfectly well read as failed
	// on the strength of the attempt before it — which is the opposite of what retaining
	// the output is for. Its output is reached through the logs endpoint instead.
	instances = slices.DeleteFunc(instances, func(instance driver.Instance) bool {
		return instance.Retained
	})

	// Health is folded into the instance states before the workload's own state is
	// derived, so a container that is up but not working reads as failed rather than
	// running — and is replaced by the same paced path a crashed one takes. Each
	// instance carries its own verdict: one failing its check must not condemn the
	// others.
	//
	// The restart policy is applied after it, on the instances that have ended. An
	// instance the policy retires is finished with, so a stale health result must not
	// reopen the question of whether it is working.
	for i := range instances {
		instances[i].State = healthState(instances[i].State, healths[instances[i].Index])
		instances[i].State = state.Completion(instances[i], policy)
	}

	// A suspended workload's occurrences will not happen, so none is reported: a
	// time a caller could wait for that the server has no intention of honouring
	// would be worse than no answer.
	var next time.Time
	if !suspended {
		next = nextRun(spec.Schedule, instances, row.UpdatedAt)
	}

	// Paired with what takt established about each of them only once their states
	// have settled, so nothing downstream can see an instance whose verdict has been
	// read but not yet applied.
	reported := make([]Instance, 0, len(instances))
	for _, instance := range instances {
		reported = append(reported, Instance{Instance: instance, Health: healths[instance.Index]})
	}

	return Workload{
		Name:      row.Name,
		Version:   row.Version,
		Runtime:   manifest.Runtime(row.Runtime),
		Spec:      spec,
		Labels:    row.Labels,
		Instances: reported,
		Ports:     newResolvedPorts(ports),
		State:     state.Of(instances, deleting, suspended),
		Deleting:  deleting,
		Suspended: suspended,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		NextRun:   next,
	}, nil
}

// nextRun reports when a scheduled workload runs again, or the zero time when it runs
// continuously. Before the first run the occurrence is counted from when the workload
// was applied, so a schedule reports its next run as soon as it exists.
//
// Derived on read rather than stored, like every other observed value: the occurrence
// is a function of the expression and the last run, both of which are already known.
func nextRun(schedule *manifest.Schedule, instances []driver.Instance, applied time.Time) time.Time {
	if schedule == nil {
		return time.Time{}
	}

	parsed, err := schedule.Parsed()
	if err != nil {
		// Validated before it was stored, so this means the specification and the
		// rules have diverged. Nothing useful can be reported.
		return time.Time{}
	}

	var last time.Time
	for _, instance := range instances {
		if instance.StartedAt.After(last) {
			last = instance.StartedAt
		}
	}

	// Counted from the last run, or from when the specification was applied for a
	// workload that has not run yet, which is what the reconciler does.
	if last.IsZero() {
		last = applied
	}

	return parsed.Next(last)
}

// newResolvedPorts maps stored allocations onto the service's view of them.
func newResolvedPorts(ports []database.Port) []ResolvedPort {
	if len(ports) == 0 {
		return nil
	}

	resolved := make([]ResolvedPort, 0, len(ports))
	for _, port := range ports {
		resolved = append(resolved, ResolvedPort{
			Instance: port.Instance,
			Name:     port.Name,
			To:       port.Container,
			From:     port.Host,
			Protocol: manifest.Protocol(port.Protocol),
			Dynamic:  port.Dynamic,
		})
	}

	return resolved
}
