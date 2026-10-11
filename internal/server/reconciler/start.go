package reconciler

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/port"
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

	// The mark follows the outcome of every attempt: set while the image is still
	// being fetched, and cleared by a start that went any other way, so a slot
	// never reads as waiting on a pull that is over.
	r.markPulling(row.Name, index, errors.Is(err, driver.ErrImagePulling))

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
		// outside takt has taken, which nothing takt does will free. Docker
		// reports that as an untyped error whose wording is not part of any
		// contract, so rather than recognise the failure, the ports are asked
		// whether they are held. Ports the specification pinned are left alone:
		// they were asked for, so moving them would be overriding a decision
		// rather than revising a guess.
		r.abandonPorts(ctx, row, index)

		return fmt.Errorf("failed to start workload: %w", err)
	}

	r.logger.With("workload", row.Name, "id", id, "instance", index, "version", row.Version).Info("workload started")
	r.record(ctx, row.Name, reason, event.Fields{Instance: index})
	r.markStarted(row.Name, index, row.Version)

	return nil
}

// abandonPorts gives up the host ports takt chose for a workload when something else
// holds one of them, so that the next pass tries different ones. Failures are logged
// rather than returned: the caller is already reporting why the workload didn't
// start, and a workload that keeps its ports is no worse off than before.
//
// The ports are probed rather than given up on every failure. A start fails for
// many reasons that have nothing to do with its ports — an image tag that does not
// exist, a registry that is down, a daemon that is not answering — and a workload's
// first instance moving is a specification change: its hash moves, every instance is
// replaced, and everything reading its address is redeployed. Paying that on every
// paced attempt of a workload whose image is misspelt churned every reader for
// nothing. A port that something else is listening on is the one failure a move
// fixes, so that is what earns one.
func (r *Reconciler) abandonPorts(ctx context.Context, row database.Workload, index int) {
	if r.reallocate == nil {
		return
	}

	// Read before the reallocation rather than after it, because afterwards these
	// are whatever the workload moved to rather than what it gave up.
	held := r.slotPorts(row, index)
	if !taken(held) {
		return
	}

	abandoned := hostPorts(held)

	changed, err := r.reallocate(ctx, row.Name, index)
	switch {
	case err != nil:
		r.logger.With("workload", row.Name, "instance", index, "error", err).Error("failed to reallocate workload ports")
	case changed:
		r.logger.With("workload", row.Name, "instance", index).Info("reallocated host ports after a failed start")

		r.record(ctx, row.Name, event.PortsAbandoned, event.Fields{Instance: index, Ports: abandoned})
	}
}

// taken reports whether something on the host is listening on any dynamic host port
// among the rows. Only dynamic ports are asked, since those are the only ones a
// reallocation would move: a pinned port that is held is the operator's to resolve.
//
// A bind probe sees every listener in the host's network namespace, which is where a
// process, a container's userland proxy or another program's socket all appear. A
// daemon publishing ports without a userland proxy holds nothing a probe can see, so
// a clash with a container takt did not start is missed there — the same blind spot
// the allocator has when it picks a port, so no port it would have chosen is one it
// now refuses to move away from.
func taken(rows []database.Port) bool {
	for _, row := range rows {
		if !row.Dynamic {
			continue
		}

		if !port.Available(port.Protocol(row.Protocol), row.Host) {
			return true
		}
	}

	return false
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
