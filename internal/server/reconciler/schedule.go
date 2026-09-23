package reconciler

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// schedule reads when a stored workload should run, reporting nil when it runs
// continuously.
//
// A specification that cannot be decoded, or an expression that cannot be parsed, is
// treated as no schedule at all. Both were validated before they were stored, so
// either means the specification and the rules have diverged — and running a workload
// continuously is a better failure than never running it again.
func (r *Reconciler) schedule(ctx context.Context, row database.Workload) cron.Schedule {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return nil
	}

	declared := spec.Schedule
	if declared == nil {
		return nil
	}

	parsed, err := declared.Parsed()
	if err != nil {
		r.logger.With("workload", row.Name, "error", err).Error("failed to parse schedule")
		r.record(ctx, row.Name, event.ScheduleInvalid, event.Fields{
			Schedule: declared.Cron,
			Error:    err.Error(),
		})

		return nil
	}

	return parsed
}

// overlap reads what a stored workload asks for when an occurrence comes due while the
// previous run is still going.
func overlap(row database.Workload) manifest.OverlapPolicy {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return manifest.OverlapReplace
	}

	declared := spec.Schedule
	if declared == nil {
		return manifest.OverlapReplace
	}

	return declared.Overlap
}

// occurrence runs a scheduled workload when its expression says to.
//
// The times come from the expression and the last run, so nothing about when a workload
// ran has to be persisted: the instance the driver reports carries the time it started,
// and a container that has ended is left in place until the next occurrence replaces
// it.
//
// Missed occurrences are missed. The next occurrence after the last run is what is
// asked for, so several passing while the server was down produce one run rather than
// one each.
func (r *Reconciler) occurrence(ctx context.Context, row database.Workload, instances []driver.Instance, schedule cron.Schedule) error {
	// The occurrence is counted from the last run, or from when the specification was
	// applied for a workload that has not run yet. A schedule says when to run, and
	// the moment of applying is not one of the times it names, so the first occurrence
	// after that is what the workload waits for.
	since := lastRun(instances)
	if since.IsZero() {
		since = row.UpdatedAt
	}

	if r.now().Before(schedule.Next(since)) {
		// Nothing is due. A run that ended stays as it is, so its outcome is readable
		// until the next occurrence replaces it.
		return r.between(ctx, row, instances)
	}

	// An occurrence is due. Anything still running is from the previous one.
	if slices.ContainsFunc(instances, running) {
		if overlap(row) == manifest.OverlapSkip {
			r.logger.With("workload", row.Name).Info("skipped an occurrence, the previous run is still going")
			r.record(ctx, row.Name, event.OccurrenceSkipped, event.Fields{})

			return nil
		}

		r.logger.With("workload", row.Name).Debug("replacing a run still going at its next occurrence")
		r.record(ctx, row.Name, event.OccurrenceReplaced, event.Fields{})
	}

	// Whatever is there is cleared first: container names derive from the workload and
	// version, so a new run would collide with the one it replaces.
	if err := r.stop(ctx, row); err != nil {
		return fmt.Errorf("failed to clear the previous run: %w", err)
	}

	r.settleAll(row.Name)

	return r.start(ctx, row, 0, event.RunStarted)
}

// between decides what to do with a scheduled workload when no occurrence is due.
//
// Only a failed run is retried. A run that ended cleanly did what the occurrence asked
// of it, and starting it again would be running the workload at a time its schedule
// does not name — which is what a schedule exists to prevent, whatever the restart
// policy would otherwise say.
//
// A failure is different: the occurrence did not achieve what it asked for, so the
// policy decides whether to try again before the next one is due.
func (r *Reconciler) between(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	if slices.ContainsFunc(instances, running) {
		return nil
	}

	// A scheduled workload waits between occurrences with its last run left in
	// place, so this is where a run that has finished is seen.
	r.ended(ctx, row, 0, instances, event.RunFinished)

	if !slices.ContainsFunc(instances, failed) {
		return nil
	}

	policy := restartPolicy(row)
	if retired(policy, instances) {
		return nil
	}

	return r.restart(ctx, row, 0, instances, event.RunStarted)
}

// lastRun reports when the workload most recently started, or the zero time when
// nothing has.
func lastRun(instances []driver.Instance) time.Time {
	var last time.Time

	for _, instance := range instances {
		if instance.StartedAt.After(last) {
			last = instance.StartedAt
		}
	}

	return last
}
