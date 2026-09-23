package reconciler

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// start runs one instance of a workload, recording it under the given reason: an
// instance started for a long-running workload, or a run started for a scheduled
// one. The caller names it because the same start answers both questions.
func (r *Reconciler) start(ctx context.Context, row database.Workload, index int, reason event.Reason) error {
	w, err := driver.NewWorkload(row, r.hostPaths)
	if err != nil {
		return err
	}

	w.Instance = index

	// The stored specification carries the first instance's resolved ports, so a
	// later instance swaps in the rows the allocator settled for its own index.
	if index > 0 {
		w.Ports = driverPorts(r.slotPorts(row, index))
	}

	// The hash the instance is stamped with is its own: the slot's expected hash,
	// which folds in the addresses this instance resolves. Staleness on a later
	// pass compares against the same computation.
	if w.SpecHash, err = r.slotHash(ctx, row, index); err != nil {
		return fmt.Errorf("failed to resolve the instance's expected hash: %w", err)
	}

	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	d, ok := r.driverFor(row)
	if !ok {
		// Nothing runs this workload's runtime, which converge has already reported.
		return nil
	}

	// The secrets and variables the workload reads are resolved here, immediately
	// before the driver is handed the environment, so that a secret's plaintext lives
	// no longer than it has to.
	//
	// Ahead of the start rather than inside its error path: a reference that cannot be
	// resolved would otherwise be treated as a workload that failed to start, which
	// gives up the host ports takt chose for it. Ports have nothing to do with why
	// this failed, and churning them would move the workload's address for a reason
	// the operator cannot see. Returning here instead leaves the backoff to pace the
	// retries, so a workload waiting on a secret does not fill the log.
	if r.env != nil {
		if w.Env, err = r.env.Resolve(startCtx, w.Env, row.ID, w.Name, w.Instance); err != nil {
			return fmt.Errorf("failed to resolve environment for workload: %w", err)
		}
	}

	// The values the workload mounts are written here, for the same reasons and on the
	// same terms as the environment above: as late as they can be, and ahead of the
	// start rather than inside its error path, so a value that cannot be read leaves
	// the workload's ports alone and lets the backoff pace the retries.
	//
	// The files join the volumes the specification already resolved, so a driver
	// mounts one exactly as it mounts the other.
	if r.mounts != nil {
		mounted, err := r.mounts.Deliver(startCtx, row.ID, row.Version, w.Spec)
		if err != nil {
			return fmt.Errorf("failed to deliver mounted values for workload: %w", err)
		}

		w.Volumes = append(w.Volumes, mounted...)
	}

	id, err := d.Start(startCtx, w)
	if err != nil {
		// An image still being fetched is a waiting state rather than a failure.
		// The instance stays pending with its ports and pacing untouched, and a
		// later pass — hurried along by the driver when the pull lands — starts
		// it.
		//
		// The event is what makes the wait visible: a workload pulling a large
		// image and one the reconciler has not reached yet both read as pending,
		// and this is what tells them apart. It repeats every pass until the pull
		// lands, which coalescing folds into one row with a climbing count.
		if errors.Is(err, driver.ErrImagePulling) {
			r.logger.With("workload", row.Name, "instance", index).Debug("waiting for image pull")
			r.record(ctx, row.Name, event.ImagePulling, event.Fields{Reference: imageOf(w.Spec)})

			return nil
		}

		// A workload that cannot start may be sitting on a host port something
		// outside takt has taken, which nothing takt does will free. Rather than
		// try to recognise that specific failure — docker reports it as an
		// untyped error whose wording is not part of any contract — any failure
		// gives up the ports takt chose for this instance. Ports the specification
		// pinned are left alone: they were asked for, so moving them would be
		// overriding a decision rather than revising a guess.
		r.abandonPorts(ctx, row, index)

		return fmt.Errorf("failed to start workload: %w", err)
	}

	r.logger.With("workload", row.Name, "id", id, "instance", index, "version", row.Version).Info("workload started")
	r.record(ctx, row.Name, reason, event.Fields{Instance: index})

	// After the start rather than before it. A replacement's files are written
	// alongside those the instance being replaced is still reading, and sweeping them
	// first would pull those out from under it if this start then failed.
	//
	// A failure here leaves plaintext on the disk that nothing reads, which is worth
	// a warning and is not worth failing a workload that started. The next start
	// sweeps it, since this removes everything but the current version rather than
	// the one it just replaced.
	if r.mounts != nil {
		if err = r.mounts.Reclaim(row.ID, row.Version); err != nil {
			r.logger.With("workload", row.Name, "error", err).
				Warn("failed to reclaim superseded mounted values")
		}
	}

	// The shared tokens of the versions this start superseded go the way their
	// files just did, and a failure is a warning for the reason the reclaim's
	// is: the next start sweeps everything but the current version again, and
	// the teardown revokes whatever remains.
	if r.tokens != nil {
		if err = r.tokens.RevokeSuperseded(ctx, row.ID, row.Version); err != nil {
			r.logger.With("workload", row.Name, "error", err).
				Warn("failed to revoke superseded workload tokens")
		}
	}

	return nil
}

// abandonPorts gives up the host ports takt chose for a workload, so that the next
// pass tries different ones. Failures are logged rather than returned: the caller is
// already reporting why the workload didn't start, and a workload that keeps its
// ports is no worse off than before.
func (r *Reconciler) abandonPorts(ctx context.Context, row database.Workload, index int) {
	if r.reallocate == nil {
		return
	}

	// Read before the reallocation rather than after it, because afterwards these
	// are whatever the workload moved to rather than what it gave up.
	abandoned := hostPorts(r.slotPorts(row, index))

	changed, err := r.reallocate(ctx, row.Name, index)
	switch {
	case err != nil:
		r.logger.With("workload", row.Name, "instance", index, "error", err).Error("failed to reallocate workload ports")
	case changed:
		r.logger.With("workload", row.Name, "instance", index).Info("reallocated host ports after a failed start")

		r.record(ctx, row.Name, event.PortsAbandoned, event.Fields{Instance: index, Ports: abandoned})
	}
}

// driverPorts maps port rows onto the shape a driver publishes.
func driverPorts(rows []database.Port) []driver.Port {
	if len(rows) == 0 {
		return nil
	}

	ports := make([]driver.Port, 0, len(rows))
	for _, row := range rows {
		ports = append(ports, driver.Port{Container: row.Container, Host: row.Host, Protocol: row.Protocol})
	}

	return ports
}

// imageOf reads the image reference a workload runs, which an event names so an
// operator can see which pull they are waiting on. A workload that runs a process
// rather than a container has none, and so reads as empty.
func imageOf(spec manifest.Spec) string {
	if spec.Container == nil {
		return ""
	}

	return spec.Container.Image
}
