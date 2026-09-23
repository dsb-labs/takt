package reconciler

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// register keeps the checker in step with the desired state, so that every workload
// declaring a check has one and no workload that has gone still does.
//
// This belongs to the pass rather than to whoever applies a workload: the reconciler
// is what runs continuously, so a server that restarts resumes checking the workloads
// it adopts without waiting for anything to be applied or read again.
func (r *Reconciler) register(rows []database.Workload, observed map[string][]driver.Instance) {
	if r.checker == nil || r.ports == nil {
		return
	}

	for _, row := range rows {
		count := countOf(row)

		byIndex := make(map[int][]driver.Instance, count)
		for _, instance := range observed[row.Name] {
			byIndex[instance.Index] = append(byIndex[instance.Index], instance)
		}

		// One check per instance, against that instance's own host port, so one
		// instance failing to answer marks that instance alone.
		for index := range count {
			check, ok, err := healthCheck(r.bind, row, r.slotPorts(row, index))
			switch {
			case err != nil:
				// The specification was validated before it was stored, so a check
				// that cannot be resolved now means the two have diverged rather
				// than that the operator made a mistake.
				r.logger.With("workload", row.Name, "instance", index, "error", err).Error("failed to resolve health check")
			case ok && row.DeletedAt.IsZero() && row.SuspendedAt.IsZero() && r.checkable(row, index, byIndex[index]):
				r.checker.Set(row.Name, index, check)
			default:
				// The workload declares no check, or is on its way out, or is
				// suspended, or this instance has ended and will not be restarted.
				// None is worth probing, and probing the last would report a
				// finished instance as unhealthy for no longer answering.
				r.checker.ForgetInstance(row.Name, index)
			}
		}
	}
}

// checkable reports whether a slot is worth probing: something in it may yet run,
// or the policy will bring something back.
//
// A slot whose instances have all stopped, under a policy that asks for nothing
// further, is not. Nor would probing it tell anything: a finished instance reads
// as unhealthy for no longer answering.
//
// An instance the checker failed is not one that stopped. The runtime still reports
// it running, and forgetting its check would have the next pass read it as running
// again, register the check again, and fail it again: a verdict flapping every pass
// for a workload whose policy said to leave it. The check stays so the verdict
// stays.
func (r *Reconciler) checkable(row database.Workload, index int, instances []driver.Instance) bool {
	if !retired(restartPolicy(row), instances) {
		return true
	}

	return slices.ContainsFunc(instances, func(instance driver.Instance) bool {
		return r.checkFailed(row.Name, index, instance.ID)
	})
}

// healthCheck resolves a stored workload's health check into something probeable,
// reporting false when the workload declares none.
func healthCheck(bind string, row database.Workload, ports []database.Port) (health.Check, bool, error) {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return health.Check{}, false, err
	}

	resolved := spec
	if resolved.Health == nil {
		return health.Check{}, false, nil
	}

	host, err := healthPort(*resolved.Health, ports)
	if err != nil {
		return health.Check{}, false, err
	}

	return health.Check{
		Address:     net.JoinHostPort(probeHost(bind), strconv.Itoa(host)),
		HTTP:        resolved.Health.HTTP,
		Interval:    resolved.Health.Interval,
		Timeout:     resolved.Health.Timeout,
		Retries:     resolved.Health.Retries,
		StartPeriod: resolved.Health.StartPeriod,
	}, true, nil
}

// probeHost returns the host a check is performed against, given the address a
// workload's ports are published on.
//
// A port published on one interface is only reachable there, so the check has to go
// where the workload actually is rather than to loopback by assumption. The exception
// is the unspecified address, which means every interface: loopback is one of them,
// and probing it keeps the check to traffic that never leaves the host.
func probeHost(bind string) string {
	if parsed := net.ParseIP(bind); parsed == nil || parsed.IsUnspecified() {
		return "127.0.0.1"
	}

	return bind
}

// healthPort finds the host port that reaches the port the check names.
//
// Only a TCP port is considered, as validation only accepts a check against one: both
// probes connect, and a connection to a UDP port succeeds whatever is behind it. A
// workload publishing 53 on both protocols is probed on its TCP side.
func healthPort(check manifest.Health, ports []database.Port) (int, error) {
	checkable := slices.DeleteFunc(slices.Clone(ports), func(port database.Port) bool {
		return port.Protocol == string(manifest.ProtocolUDP)
	})

	if len(checkable) == 0 {
		return 0, fmt.Errorf("workload publishes no %s port to check", manifest.ProtocolTCP)
	}

	// Validation requires the port to be named when several are published, so a
	// check naming none can only mean the single port the workload has.
	if check.Port == "" {
		return checkable[0].Host, nil
	}

	for _, port := range checkable {
		if check.Port.Matches(port.Name, port.Container) {
			return port.Host, nil
		}
	}

	return 0, fmt.Errorf("port %q is not published by the workload over %s", check.Port, manifest.ProtocolTCP)
}

// checked folds what takt's health check established into an instance's state, so
// that a workload the driver reports as running but which cannot serve converges
// instead of being left alone.
//
// A failing check makes the instance failed, which routes it into the same paced
// restart a crashed container takes: the reaction to "not working" is the same
// whether the process died or merely stopped answering. A check that has not passed
// yet makes it pending, which the pass treats as up — a workload still starting
// must not be replaced for not having answered yet.
func (r *Reconciler) checked(ctx context.Context, instance driver.Instance) driver.Instance {
	if r.checker == nil || instance.State != driver.StateRunning {
		return instance
	}

	result, ok := r.checker.Result(instance.Workload, instance.Index)
	if !ok {
		return instance
	}

	r.health(ctx, instance, result)

	switch result.Status {
	case health.StatusUnhealthy:
		r.failCheck(instance)

		instance.State = driver.StateFailed
	case health.StatusStarting:
		instance.State = driver.StatePending
	}

	return instance
}

// health records the edges an instance's check crosses, which is what tells a
// replacement caused by a workload that stopped answering from one caused by a
// crash.
//
// Only a change is recorded. A check runs continuously and a pass reads its most
// recent verdict, so recording the verdict itself would report on every pass how
// the workload is rather than when it changed.
//
// The verdict is read where the pass reads it rather than where the probe writes
// it, so a check that fails and recovers between two passes is not recorded. That
// is the price of keeping this off the probe's path, which writes its result while
// holding the checker's lock.
func (r *Reconciler) health(ctx context.Context, instance driver.Instance, result health.Result) {
	previous, changed := r.verdict(instance.Workload, instance.Index, result.Status)
	if !changed {
		return
	}

	switch {
	case result.Status == health.StatusUnhealthy:
		r.record(ctx, instance.Workload, event.HealthCheckFailing, event.Fields{
			Instance: instance.Index,
			Count:    result.Failures,
			Error:    result.Error,
		})
	case result.Status == health.StatusHealthy && previous == health.StatusUnhealthy:
		// Recovery is only recorded against a failure this saw. A workload passing
		// its first check has not recovered from anything, and saying so on every
		// workload that starts would bury the ones that did.
		r.record(ctx, instance.Workload, event.HealthCheckRecovered, event.Fields{Instance: instance.Index})
	}
}

// verdict reports the status an instance's check last held and whether the one
// given differs from it, remembering the new one either way.
//
// A status never seen before counts as a change, so a workload adopted while
// already failing is reported rather than passed over for having always been that
// way.
func (r *Reconciler) verdict(workload string, index int, status health.Status) (health.Status, bool) {
	r.mux.Lock()
	defer r.mux.Unlock()

	key := slot{workload: workload, instance: index}

	previous, seen := r.verdicts[key]
	if seen && previous == status {
		return previous, false
	}

	r.verdicts[key] = status

	return previous, true
}

// ended records how an instance finished, once per instance rather than once per
// pass that sees it finished.
//
// The reason is the caller's because the two answer different questions: a
// scheduled workload ending is a run that finished, and a long-running one ending
// is an instance that exited when it was meant to stay up. An instance the checker
// failed is neither: its process is still running, so it is recorded as unhealthy
// with what the check reported rather than with an exit status it does not have.
//
// Nothing is recorded for a slot with nothing ended, which is the ordinary case and
// so is the first thing checked.
func (r *Reconciler) ended(ctx context.Context, row database.Workload, index int, instances []driver.Instance, reason event.Reason) {
	last, ok := lastEnded(instances)
	if !ok || !r.firstSighting(row.Name, index, last.ID) {
		return
	}

	if r.checkFailed(row.Name, index, last.ID) {
		result, _ := r.checker.Result(row.Name, index)

		r.record(ctx, row.Name, event.InstanceUnhealthy, event.Fields{
			Instance: index,
			Count:    result.Failures,
			Error:    result.Error,
		})

		return
	}

	code := last.ExitCode

	r.record(ctx, row.Name, reason, event.Fields{Instance: index, ExitCode: &code})
}

// failCheck remembers that the checker failed an instance, so the ending the pass
// goes on to see is attributed to the check rather than to the process.
func (r *Reconciler) failCheck(instance driver.Instance) {
	r.mux.Lock()
	defer r.mux.Unlock()

	r.unhealthy[slot{workload: instance.Workload, instance: instance.Index}] = instance.ID
}

// checkFailed reports whether an ended instance is one the checker failed rather
// than one whose process stopped.
func (r *Reconciler) checkFailed(workload string, index int, id string) bool {
	r.mux.Lock()
	defer r.mux.Unlock()

	return r.unhealthy[slot{workload: workload, instance: index}] == id
}

// firstSighting reports whether an ended instance is one whose ending has not been
// recorded yet, remembering it when it is.
func (r *Reconciler) firstSighting(workload string, index int, id string) bool {
	r.mux.Lock()
	defer r.mux.Unlock()

	key := slot{workload: workload, instance: index}
	if r.exits[key] == id {
		return false
	}

	r.exits[key] = id

	return true
}

// lastEnded returns the instance in a slot that ran most recently and is no longer
// running, reporting false when every instance is still up.
func lastEnded(instances []driver.Instance) (driver.Instance, bool) {
	var (
		last  driver.Instance
		found bool
	)

	for _, instance := range instances {
		if running(instance) || terminating(instance) {
			continue
		}

		if !found || instance.StartedAt.After(last.StartedAt) {
			last = instance
			found = true
		}
	}

	return last, found
}
