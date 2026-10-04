package reconciler

import (
	"bytes"
	"context"
	"slices"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/internal/server/resolve"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// notReady reports whether a slot's first start should be held because an instance
// of a workload it references has not yet passed its health check, recording why.
//
// This is not a declared dependency. The dependency is the reference the manifest
// already writes, and the readiness signal is the check the target already runs.
// A target with no check is not waited for, because there is nothing to wait on,
// and a reference that cannot be resolved keeps its existing failure path rather
// than becoming a readiness question.
//
// Only a first start under a version is held. A restart after a crash and a
// replacement after a change are not: the reader was talking to its target a
// moment ago, and holding a replacement would let a slow dependency stall a
// rollout that had nothing to do with it. The restart path has the backoff to
// pace it, where a hold records no attempt and widens nothing.
//
// The wait is bounded. A target that never passes must not hold its readers
// forever, so once a slot has been held for the configured wait it starts anyway
// and fails on its own terms, which is what it did before the gate existed. Giving
// up is remembered under the version the way a start is, since the start that
// follows may fail: without that, the next pass would find nothing started and
// hold the slot for the full wait again, so a reader whose target never passes
// and whose start fails would cycle through the wait rather than the backoff.
func (r *Reconciler) notReady(ctx context.Context, row database.Workload, index int) bool {
	if r.readiness == 0 || r.checker == nil || r.env == nil {
		return false
	}

	// The marker is present exactly when a reference exists, the trick slotHash
	// uses, so a workload referencing nothing costs no decode. A file rendered
	// from a variable may name a workload too, and the stored bytes are canonical
	// JSON, so the key that asks for that is present verbatim when one does.
	if !bytes.Contains(row.Spec, []byte("${workload:")) && !bytes.Contains(row.Spec, []byte(`"expand":true`)) {
		return false
	}

	key := slot{workload: row.Name, instance: index}

	r.mux.Lock()
	version, ok := r.started[key]
	r.mux.Unlock()

	if ok && version == row.Version {
		return false
	}

	target, result, blocked := r.blocker(ctx, row, index)
	if !blocked {
		r.release(key)

		return false
	}

	now := r.now()

	r.mux.Lock()
	since, ok := r.held[key]
	if !ok {
		since = now
		r.held[key] = since
	}
	r.mux.Unlock()

	fields := event.Fields{
		Instance:       index,
		Name:           target.Workload,
		TargetInstance: target.Instance,
	}

	if now.Sub(since) >= r.readiness {
		r.logger.With("workload", row.Name, "instance", index, "target", target.Workload, "target_instance", target.Instance).
			Info("starting an instance without waiting any longer for its dependency")

		fields.Delay = r.readiness
		r.record(ctx, row.Name, event.DependencyWaitGivenUp, fields)
		r.markStarted(row.Name, index, row.Version)

		return false
	}

	r.logger.With("workload", row.Name, "instance", index, "target", target.Workload, "target_instance", target.Instance).
		Debug("holding an instance until its dependency passes its health check")

	// Recorded every pass the slot stays held and coalesced into one row, so the
	// count says how long the wait has been.
	fields.Error = result.Error
	r.record(ctx, row.Name, event.DependencyNotReady, fields)

	return true
}

// blocker returns the first referenced instance that declares a health check and
// has not passed it, and that instance's most recent result.
//
// Every reference is consulted, so a reader of two targets waits on both. The
// target's specification comes from the rows the pass read, since the pass
// already holds every workload and asking the repository again per slot would
// cost a query per reference per pass.
func (r *Reconciler) blocker(ctx context.Context, row database.Workload, index int) (resolve.Target, health.Result, bool) {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return resolve.Target{}, health.Result{}, false
	}

	targets, err := r.env.Targets(ctx, spec.Env, row.Name, index)
	if err != nil {
		// Reported by the start that follows, which resolves the same references
		// and records what could not be resolved.
		return resolve.Target{}, health.Result{}, false
	}

	// A file rendered from a variable is shared by every instance of the version,
	// so what it names resolves as the first instance rather than as this one.
	if r.mounts != nil {
		contents, err := r.mounts.Contents(ctx, spec)
		if err != nil {
			return resolve.Target{}, health.Result{}, false
		}

		mounted, err := r.env.Targets(ctx, contents, row.Name, 0)
		if err != nil {
			return resolve.Target{}, health.Result{}, false
		}

		for _, target := range mounted {
			if !slices.Contains(targets, target) {
				targets = append(targets, target)
			}
		}
	}

	rows := r.rows.Load()
	if rows == nil {
		return resolve.Target{}, health.Result{}, false
	}

	for _, target := range targets {
		targetRow, ok := (*rows)[target.Workload]
		if !ok || !checked(targetRow) {
			continue
		}

		result, ok := r.checker.Result(target.Workload, target.Instance)
		if ok && result.Status == health.StatusHealthy {
			continue
		}

		return target, result, true
	}

	return resolve.Target{}, health.Result{}, false
}

// checked reports whether a stored workload declares a health check.
func checked(row database.Workload) bool {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return false
	}

	return spec.Health != nil
}

// release forgets that a slot is held, once it is started or no longer blocked.
func (r *Reconciler) release(key slot) {
	r.mux.Lock()
	defer r.mux.Unlock()

	delete(r.held, key)
}

// markStarted records that the gate is finished with a slot under a version, because
// it started or because the wait was given up, so that its next empty pass is a
// restart rather than a first start and is not held.
func (r *Reconciler) markStarted(workload string, index, version int) {
	r.mux.Lock()
	defer r.mux.Unlock()

	key := slot{workload: workload, instance: index}
	r.started[key] = version

	delete(r.held, key)
}

// forgetStarted forgets everything the readiness gate remembers about a workload,
// once nothing of it remains. Later than settleAll: a suspended workload resumes
// under the version it started, and its resume is not a first start.
func (r *Reconciler) forgetStarted(workload string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	for key := range r.started {
		if key.workload == workload {
			delete(r.started, key)
			delete(r.held, key)
		}
	}

	for key := range r.pulling {
		if key.workload == workload {
			delete(r.pulling, key)
		}
	}
}
