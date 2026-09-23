package reconciler

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
)

// teardown removes a workload that has been marked for deletion, and its desired
// state once the driver reports nothing is left.
//
// The row is the last thing to go. While it exists the workload reads as
// terminating, so the teardown is observable. Once the driver is empty there is
// nothing left for the row to describe, and removing it is what finally makes the
// workload disappear. Ordering it this way means a failure at any point leaves a
// workload that will be torn down again on the next pass, rather than running work
// that nothing records.
func (r *Reconciler) teardown(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	if len(instances) > 0 {
		// Already on its way out from an earlier pass; stopping it again would just
		// race the runtime finishing the job.
		if slices.ContainsFunc(instances, terminating) {
			r.logger.With("workload", row.Name).Debug("waiting for deleted workload to finish terminating")

			return nil
		}

		r.logger.With("workload", row.Name).Debug("stopping deleted workload")

		// Discarded rather than stopped, even here where something is still running.
		// Stopping keeps the instance for its output, and the driver would then never
		// report the workload as gone — so the pass below that removes the row would
		// never be reached and a deleted workload would be torn down forever.
		//
		// A workload on its way out has no reader for its output at any stage, which is
		// what makes this the right call rather than a workaround for that loop.
		if err := r.discard(ctx, row); err != nil {
			return fmt.Errorf("failed to stop deleted workload: %w", err)
		}

		// The stop may not have taken effect yet, so the row is left for the next
		// pass to reap once the driver reports the work is gone.
		return nil
	}

	// Nothing is running, but a driver may still hold what it kept for a workload it has
	// no live instance for — an attempt stopped before the delete, or one this pass has
	// just discarded and not yet observed as gone.
	//
	// Before the row goes, for the same reason the mounted values are: once it is gone
	// there is no identifier to find either by.
	if err := r.discard(ctx, row); err != nil {
		return err
	}

	// Nothing is running, so the values the workload mounted have no reader left. This
	// is what takes a mounted secret's plaintext off the disk, and it happens before
	// the row goes: once that is gone there is no identifier to find the files by, and
	// only the periodic prune would ever remove them.
	if r.mounts != nil {
		if err := r.mounts.Forget(row.ID); err != nil {
			return fmt.Errorf("failed to remove mounted values: %w", err)
		}
	}

	// The tokens minted for the workload have no holder left either. Revoked
	// before the row goes for the reason the files are removed before it: the
	// identifier is what finds them, though deleting the row would take them
	// with it anyway.
	if r.tokens != nil {
		if err := r.tokens.RevokeForWorkload(ctx, row.ID); err != nil {
			return fmt.Errorf("failed to revoke workload tokens: %w", err)
		}
	}

	if err := r.workloads.Delete(ctx, row.Name); err != nil {
		return fmt.Errorf("failed to delete workload: %w", err)
	}

	// Backoff would otherwise outlive the workload, pacing the restarts of a later
	// workload that happens to reuse the name.
	r.settleAll(row.Name)

	// A restart asked for before the deletion landed dies with the workload, for
	// the same reason: it would otherwise lie in wait for a later workload that
	// reuses the name.
	r.restartRequested(row.Name)

	r.logger.With("workload", row.Name).Info("workload deleted")

	return nil
}

// suspend holds a workload down while it is marked as suspended.
//
// This mirrors teardown without the removal of desired state. The instances are
// stopped, and once the driver reports nothing running the values the workload
// mounted are removed — they have no reader left, and a suspended workload
// leaving secret plaintext on the disk is the condition prune exists to clean
// up. The row, the retained instance and its output all stay, so what the
// workload last did remains readable while it is down.
func (r *Reconciler) suspend(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	// Already on its way out from an earlier pass. Stopping it again would just
	// race the runtime finishing the job.
	if slices.ContainsFunc(instances, terminating) {
		r.logger.With("workload", row.Name).Debug("waiting for suspended workload to finish terminating")

		return nil
	}

	// Only what is up is stopped. An instance that has already ended stays exactly
	// as it is — it is what keeps the workload's last output readable while it is
	// down, and it remains the workload's current instance in the driver's eyes, so
	// stopping it again would repeat the stop on every pass forever.
	if slices.ContainsFunc(instances, running) {
		r.logger.With("workload", row.Name).Debug("stopping suspended workload")

		// Stopped rather than discarded: unlike a deletion the workload comes back.
		return r.stop(ctx, row)
	}

	// Nothing is running, so the values the workload mounted have no reader left.
	// Forgetting is idempotent, so a pass repeating this while the workload stays
	// suspended removes nothing twice.
	if r.mounts != nil {
		if err := r.mounts.Forget(row.ID); err != nil {
			return fmt.Errorf("failed to remove mounted values: %w", err)
		}
	}

	// The tokens go with the files: a suspended workload holds no credential, and
	// a resume mints fresh ones as its instances start. Revocation is idempotent,
	// as forgetting is.
	if r.tokens != nil {
		if err := r.tokens.RevokeForWorkload(ctx, row.ID); err != nil {
			return fmt.Errorf("failed to revoke workload tokens: %w", err)
		}
	}

	// Backoff describes attempts to run the workload, which is exactly what
	// suspension asks to stop. Clearing it means a resume starts from a clean slate
	// rather than inside a backoff window.
	r.settleAll(row.Name)

	// A restart asked for before the suspension landed is superseded by it. What
	// was running is stopped either way, and a resume should not replay it.
	r.restartRequested(row.Name)

	return nil
}

// stop asks the driver that runs a workload to stop it, bounded so that a daemon which
// never answers costs one pass rather than blocking every workload behind it.
//
// Only that driver is asked. Asking every driver cost a round trip per workload per
// pass to runtimes that were never going to have anything — measured as the dominant
// cost of tearing down a hundred mixed workloads, where each pass repeated the waste
// for every workload still left.
func (r *Reconciler) stop(ctx context.Context, row database.Workload) error {
	d, ok := r.driverFor(row)
	if !ok {
		// Nothing runs this runtime, so nothing can be running for it. The check is
		// still dropped, since the workload is on its way out either way.
		r.forget(row.Name)

		return nil
	}

	// One deadline per instance rather than per workload. The driver stops each
	// instance's containers in turn, so a count of three under a slow daemon has
	// a third of the time per container that a count of one does — and a budget
	// that expires mid-workload turns the rest into work for the next pass.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(countOf(row))*driverTimeout)
	defer cancel()

	if err := d.Stop(ctx, row.ID, row.Name); err != nil {
		return fmt.Errorf("failed to stop workload on the %s runtime: %w", d.Name(), err)
	}

	r.forget(row.Name)

	return nil
}

// stopInstance asks the driver that runs a workload to stop one of its instances,
// bounded as stop is.
//
// The instance's check history goes with it, for the reason the workload-wide stop
// drops every check: it describes work that no longer exists, and a replacement
// condemned for the departed instance's failures could never demonstrate recovery.
func (r *Reconciler) stopInstance(ctx context.Context, row database.Workload, index int) error {
	d, ok := r.driverFor(row)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	if err := d.StopInstance(ctx, row.ID, row.Name, index); err != nil {
		return fmt.Errorf("failed to stop instance on the %s runtime: %w", d.Name(), err)
	}

	// The instance's own tokens die with it. A replacement of the same slot
	// mints fresh ones as it starts, so revoking here is what makes a rolling
	// replacement rotate an env-form credential.
	if r.tokens != nil {
		if err := r.tokens.RevokeForInstance(ctx, row.ID, index); err != nil {
			return fmt.Errorf("failed to revoke instance tokens: %w", err)
		}
	}

	if r.checker != nil {
		r.checker.ForgetInstance(row.Name, index)
	}

	return nil
}

// discardInstance removes one instance a workload's count no longer asks for, and
// everything the pass remembers about it.
func (r *Reconciler) discardInstance(ctx context.Context, row database.Workload, index int) error {
	d, ok := r.driverFor(row)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	if err := d.DiscardInstance(ctx, row.ID, row.Name, index); err != nil {
		return fmt.Errorf("failed to discard instance on the %s runtime: %w", d.Name(), err)
	}

	// An instance the count no longer asks for is not replaced, so its tokens
	// are revoked here or never.
	if r.tokens != nil {
		if err := r.tokens.RevokeForInstance(ctx, row.ID, index); err != nil {
			return fmt.Errorf("failed to revoke instance tokens: %w", err)
		}
	}

	// Everything the pass remembered about the slot goes with it. A verdict left
	// behind would have a later instance in the slot read as recovering from a
	// failure it never had, and an ending left behind would go unrecorded if the
	// runtime handed the identifier out again.
	r.mux.Lock()
	key := slot{workload: row.Name, instance: index}
	delete(r.backoff, key)
	delete(r.exits, key)
	delete(r.verdicts, key)
	delete(r.unhealthy, key)
	r.mux.Unlock()

	if r.checker != nil {
		r.checker.ForgetInstance(row.Name, index)
	}

	return nil
}

// stopOrphan removes work nothing asked for, which means asking every driver.
//
// An orphan has no stored workload by definition, so there is no runtime to read and
// no way to know which driver owns it. A driver with nothing for the name does nothing,
// so asking all of them is the only way to be sure it is gone.
//
// Discarded rather than stopped. Nothing asked for this work, so there is nobody to read
// the output of it, and an instance kept for that reason would be found again on every
// pass from here on.
func (r *Reconciler) stopOrphan(ctx context.Context, workload string) error {
	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	for _, d := range r.drivers {
		// An orphan has no stored workload, so there is no identifier to give.
		if err := d.Discard(ctx, "", workload); err != nil {
			return fmt.Errorf("failed to stop workload on the %s runtime: %w", d.Name(), err)
		}
	}

	r.forget(workload)

	return nil
}

// discard asks the driver that runs a workload to remove everything it holds for it,
// including whatever it kept for its output.
//
// Bounded like stop, and only that driver is asked, for the reasons stop gives.
func (r *Reconciler) discard(ctx context.Context, row database.Workload) error {
	d, ok := r.driverFor(row)
	if !ok {
		// Nothing runs this runtime, so nothing can be holding anything for it.
		return nil
	}

	// One deadline per instance, for the reason stop gives.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(countOf(row))*driverTimeout)
	defer cancel()

	if err := d.Discard(ctx, row.ID, row.Name); err != nil {
		return fmt.Errorf("failed to discard workload on the %s runtime: %w", d.Name(), err)
	}

	return nil
}

// forget drops a workload's health check, once the work it described is gone.
//
// The check history describes work that no longer exists. Keeping it would condemn the
// replacement for failures the departed container produced, and deny it the start
// period a newly started workload is owed — so a workload replaced for being unhealthy
// could never demonstrate that it had recovered.
func (r *Reconciler) forget(workload string) {
	if r.checker != nil {
		r.checker.Forget(workload)
	}
}
