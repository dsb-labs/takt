package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The backoff type paces restarts of a workload that keeps failing, so that a
	// container crashing in a loop doesn't spin the reconciler or the daemon.
	backoff struct {
		// The number of consecutive restarts attempted.
		attempts int
		// The earliest time the next restart may be attempted.
		next time.Time
		// How long the wait ending at next is. Kept alongside it so an event can
		// name the delay rather than infer it from the clock.
		wait time.Duration
		// Whether the workload has been given up on. The decision repeats on
		// every pass over a workload that stays given up, and this is what lets
		// it count once.
		gaveUp bool
	}
)

var (
	// errPaced marks a start that failed and was paced, so the pass that reports
	// the failure knows the pacing already recorded it.
	errPaced = errors.New("start paced")
)

const (
	// The delay before the first restart of a failed instance, doubled on each
	// consecutive failure up to maxBackoff.
	baseBackoff = time.Second
	// The ceiling on restart backoff, so a persistently broken workload is still
	// retried periodically.
	maxBackoff = 2 * time.Minute
	// How long an instance has to have been running before starting it counts as
	// having worked, and the workload's restart backoff is cleared.
	//
	// Long enough to outlast a container that exits as soon as it starts, and short
	// enough that a workload which restarts legitimately isn't paced as though it
	// were failing.
	settlePeriod = 10 * time.Second
)

// attempt starts an instance that has nothing running, pacing repeated failures
// with the same backoff a repeatedly-crashing instance gets.
//
// An instance can fail to start for reasons no amount of retrying will fix — an
// image that does not exist, a host port held by something outside takt and no free
// port to move to. Without pacing, it is retried on every pass and every driver
// event, which was measured filling the log at over a thousand errors in four
// minutes while achieving nothing.
func (r *Reconciler) attempt(ctx context.Context, row database.Workload, index int) error {
	if r.waiting(row.Name, index) {
		return nil
	}

	if err := r.start(ctx, row, index, event.InstanceStarted); err != nil {
		state := r.hold(ctx, row.Name, index, restartPolicy(row))
		r.paced(ctx, row.Name, state, err)

		return fmt.Errorf("%w: %w", errPaced, err)
	}

	r.settle(row.Name, index)

	return nil
}

// restart brings back an instance whose runs have all stopped, pacing repeated
// failures with exponential backoff. The start is recorded under the given reason,
// which the caller names for the same reason start's callers do: a scheduled run
// retried between occurrences is a run rather than an instance.
func (r *Reconciler) restart(ctx context.Context, row database.Workload, index int, instances []driver.Instance, reason event.Reason) error {
	if r.waiting(row.Name, index) {
		return nil
	}

	policy := restartPolicy(row)

	// An instance told to give up gives up. It is left exactly as it ended, so the
	// outcome stays readable, and changing the specification starts it again.
	if !policy.Restarts(failureCodeOf(instances), r.attempts(row.Name, index)) {
		// Inside the branch that reports the decision as new, not beside the log
		// line below: giving up repeats on every pass over an instance that stays
		// down, and an event recorded out here would go on being seen forever.
		if r.giveUp(row.Name, index) {
			r.instruments.giveups.Add(ctx, 1,
				metric.WithAttributes(attribute.String("workload", row.Name)))

			r.record(ctx, row.Name, event.RestartGaveUp, event.Fields{
				Instance: index,
				Count:    r.attempts(row.Name, index),
			})
		}

		r.logger.With("workload", row.Name, "instance", index, "attempts", r.attempts(row.Name, index)).
			Info("giving up on an instance that will not stay up")

		return nil
	}

	// The stopped instance has to be cleared before new work can take its place:
	// container names derive from the workload, version and instance, so a
	// replacement would otherwise collide with the corpse.
	if err := r.stopInstance(ctx, row, index); err != nil {
		return fmt.Errorf("failed to clear stopped instance: %w", err)
	}

	if err := r.start(ctx, row, index, reason); err != nil {
		state := r.hold(ctx, row.Name, index, policy)
		r.paced(ctx, row.Name, state, err)

		return fmt.Errorf("%w: %w", errPaced, err)
	}

	state := r.hold(ctx, row.Name, index, policy)

	r.logger.With(
		"workload", row.Name,
		"instance", index,
		"attempts", state.attempts,
		"exit_code", exitCodeOf(instances),
	).Debug("restarted stopped instance")

	return nil
}

// waiting reports whether an instance is still inside its backoff window.
func (r *Reconciler) waiting(workload string, index int) bool {
	r.mux.Lock()
	defer r.mux.Unlock()

	state := r.backoff[slot{workload: workload, instance: index}]

	return !state.next.IsZero() && r.now().Before(state.next)
}

// hold records another attempt against an instance and pushes out the earliest time
// the next one may happen.
func (r *Reconciler) hold(ctx context.Context, workload string, index int, restart *manifest.Restart) backoff {
	r.mux.Lock()
	defer r.mux.Unlock()

	key := slot{workload: workload, instance: index}

	state := r.backoff[key]

	state.attempts++
	state.wait = delay(state.attempts, restart.Delay)
	state.next = r.now().Add(state.wait)
	r.backoff[key] = state

	r.instruments.restarts.Add(ctx, 1,
		metric.WithAttributes(attribute.String("workload", workload)))

	return state
}

// paced records that an instance's next attempt is being held off, naming the
// wait, which attempt it paces, and the failure that earned it.
//
// Separate from hold, which does the pacing, because hold runs under r.mux and no
// event may be recorded while that is held.
func (r *Reconciler) paced(ctx context.Context, workload string, state backoff, err error) {
	r.record(ctx, workload, event.RestartPaced, event.Fields{
		Count: state.attempts,
		Delay: state.wait,
		Error: err.Error(),
	})
}

// giveUp marks an instance as given up on, reporting whether it was not already —
// the decision repeats on every pass over an instance that stays down, and this is
// what lets it count once.
func (r *Reconciler) giveUp(workload string, index int) bool {
	r.mux.Lock()
	defer r.mux.Unlock()

	key := slot{workload: workload, instance: index}

	state := r.backoff[key]
	if state.gaveUp {
		return false
	}

	state.gaveUp = true
	r.backoff[key] = state

	return true
}

// attempts reports how many restarts an instance has been given.
func (r *Reconciler) attempts(workload string, index int) int {
	r.mux.Lock()
	defer r.mux.Unlock()

	return r.backoff[slot{workload: workload, instance: index}].attempts
}

// settle forgets an instance's backoff, which is what starting from a clean slate
// means: the next failure is paced from the beginning rather than from where the
// last run of failures left off.
func (r *Reconciler) settle(workload string, index int) {
	r.mux.Lock()
	defer r.mux.Unlock()

	delete(r.backoff, slot{workload: workload, instance: index})
}

// settleAll forgets every instance's backoff, for the paths that act on the whole
// workload: a teardown, a suspension, a schedule.
func (r *Reconciler) settleAll(workload string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	for key := range r.exits {
		if key.workload == workload {
			delete(r.exits, key)
		}
	}

	for key := range r.verdicts {
		if key.workload == workload {
			delete(r.verdicts, key)
		}
	}

	for key := range r.unhealthy {
		if key.workload == workload {
			delete(r.unhealthy, key)
		}
	}

	for key := range r.backoff {
		if key.workload == workload {
			delete(r.backoff, key)
		}
	}
}

// settled reports whether an instance has been running long enough to treat starting
// it as having achieved something.
//
// Starting a container is not evidence that it works: one that exits immediately is
// observed as running in the instant between the two, so "is it up" and "did starting
// it help" are different questions. Staying up is the answer to the second, and is
// what clears a workload's restart backoff.
//
// An instance whose start time the driver didn't report is taken at face value rather
// than held against it, since the alternative is never clearing the backoff of a
// workload that is running perfectly well.
func settled(instance driver.Instance, now time.Time) bool {
	if instance.State != driver.StateRunning {
		return false
	}

	return instance.StartedAt.IsZero() || now.Sub(instance.StartedAt) >= settlePeriod
}

// delay returns how long to wait before the given restart attempt, doubling with
// each consecutive failure up to maxBackoff. The first attempt waits the base
// itself, which is what the manifest's delay promises.
func delay(attempts int, base time.Duration) time.Duration {
	if base <= 0 {
		base = baseBackoff
	}

	d := base << min(max(attempts-1, 0), 8)
	if d > maxBackoff || d <= 0 {
		// The shift overflows for a base a workload could legitimately name, so the
		// ceiling catches that as well as an ordinary long wait.
		return maxBackoff
	}

	return d
}
