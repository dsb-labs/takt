package reconciler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/mount"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The staleness type says why a slot's instances no longer match what is
	// wanted, which is the question an operator asks of a replacement they did not
	// expect.
	//
	// It travels beside the instances rather than being worked out again where the
	// replacement happens, because only the comparison that found them stale knows
	// which of the two it was.
	staleness struct {
		reason event.Reason
		fields event.Fields
	}
)

// converge brings a single workload's running state into line with its desired
// state, one instance at a time.
//
// Whole-workload questions — deletion, suspension, a missing driver, an operator's
// restart, a schedule — are answered first, because they override whatever any one
// instance is doing. Everything else is decided per instance: each slot from zero
// to count-1 is observed, replaced, restarted and paced on its own, so one crashing
// instance never touches its siblings.
func (r *Reconciler) converge(ctx context.Context, row database.Workload, instances []driver.Instance, indexes map[int]struct{}) error {
	// A workload marked for deletion is torn down here rather than by whoever
	// asked, so that one component is responsible for touching the runtime and the
	// desired state survives until the work described by it is actually gone.
	if !row.DeletedAt.IsZero() {
		return r.teardown(ctx, row, instances)
	}

	// A suspended workload is held down rather than converged. Checked ahead of
	// everything else a pass would enforce, because every branch below exists to
	// keep the workload running and suspension asks for exactly the opposite.
	if !row.SuspendedAt.IsZero() {
		return r.suspend(ctx, row, instances)
	}

	// A workload whose runtime nothing runs is stored and left alone, so it starts
	// working when its driver arrives rather than being reported as broken.
	if _, ok := r.driverFor(row); !ok {
		r.logger.With("workload", row.Name, "runtime", row.Runtime).Debug("no driver for runtime")

		return nil
	}

	count := countOf(row)

	// An operator asked for the instances to be replaced. Every slot gets the same
	// stop-then-start a stale instance gets, from the unchanged specification.
	if r.restartRequested(row.Name) {
		if slices.ContainsFunc(instances, terminating) {
			r.logger.With("workload", row.Name).Debug("waiting for workload to finish terminating")

			return nil
		}

		r.logger.With("workload", row.Name).Info("restarting workload on request")
		r.record(ctx, row.Name, event.RestartRequested, event.Fields{})

		if err := r.stop(ctx, row); err != nil {
			return fmt.Errorf("failed to stop workload for restart: %w", err)
		}

		return r.startAll(ctx, row, count)
	}

	// A scheduled workload runs when its expression says to and waits in between, so
	// the schedule decides rather than the restart policy. Validation refuses a
	// schedule with a count above one, so the whole path converges a single
	// instance.
	if schedule := r.schedule(ctx, row); schedule != nil {
		if slices.ContainsFunc(instances, terminating) {
			r.logger.With("workload", row.Name).Debug("waiting for workload to finish terminating")

			return nil
		}

		return r.occurrence(ctx, row, instances, schedule)
	}

	// A slot at or past the count is one a smaller count removed. Discarded rather
	// than stopped: the instance is not being replaced, so nothing will read the
	// output a retained corpse keeps. Retained remnants count here, which is what
	// the unfiltered indexes are for.
	for index := range indexes {
		if index < count {
			continue
		}

		r.logger.With("workload", row.Name, "instance", index).Info("removing an instance the count no longer asks for")

		if err := r.discardInstance(ctx, row, index); err != nil {
			return err
		}

		r.record(ctx, row.Name, event.InstanceRemoved, event.Fields{Instance: index})
	}

	byIndex := make(map[int][]driver.Instance, count)
	for _, instance := range instances {
		byIndex[instance.Index] = append(byIndex[instance.Index], instance)
	}

	// At most one replacement of something running at a time, so a change rolls
	// across the instances instead of taking them all down at once. A pass runs
	// on every event the runtime reports, the replacement's own start included,
	// so one per pass alone would roll at whatever speed the runtime answers.
	// A replacement counts as in flight until what it started has settled: up
	// for the settle period, and past its health check when it declares one. A
	// replacement that never settles holds the roll at one instance, which is
	// what makes a bad change a degradation rather than an outage. Slots with
	// nothing running are not held back by it.
	var (
		replaced   = r.rolling(ctx, row, count, byIndex)
		anyRunning bool
	)

	for index := range count {
		up, err := r.convergeSlot(ctx, row, index, byIndex[index], &replaced)
		if err != nil {
			return err
		}

		anyRunning = anyRunning || up
	}

	if !anyRunning {
		return nil
	}

	// A workload mounting a value it asked to be signalled about is told here,
	// because its specification is current by construction: such a value stays out
	// of the hash, so a change to one leaves the workload looking exactly as it
	// does now. Comparing what was delivered against what takt holds is the only
	// thing that would notice.
	return r.refresh(ctx, row)
}

// rolling reports whether a replacement is still in flight: some slot holds an
// instance that is current, so it is the outcome of a replacement rather than the
// subject of one, and that has not yet settled.
//
// The states the health check folds in count: an instance still in its start
// period reads as pending, and one failing its check reads as failed, and neither
// has settled. A slot whose expected hash cannot be resolved is left out, since
// staleSlot leaves such a slot alone either way.
func (r *Reconciler) rolling(ctx context.Context, row database.Workload, count int, byIndex map[int][]driver.Instance) bool {
	now := r.now()

	for index := range count {
		expected, err := r.slotHash(ctx, row, index)
		if err != nil {
			continue
		}

		for _, instance := range byIndex[index] {
			if instance.SpecHash != expected || instance.Retained {
				continue
			}

			if !settled(instance, now) {
				return true
			}
		}
	}

	return false
}

// convergeSlot brings one instance of a workload into line, reporting whether the
// slot has something up.
func (r *Reconciler) convergeSlot(ctx context.Context, row database.Workload, index int, instances []driver.Instance, replaced *bool) (bool, error) {
	// An instance on its way out is mid-teardown from an earlier pass. Acting now
	// would mean stopping what is already stopping, so the slot is left alone and
	// picked up once the runtime has finished. Only this slot waits: the others
	// have names and ports of their own to converge against.
	if slices.ContainsFunc(instances, terminating) {
		r.logger.With("workload", row.Name, "instance", index).Debug("waiting for instance to finish terminating")

		return true, nil
	}

	// Reported before anything is decided, so the ending stands whatever follows it:
	// a restart, a retirement, or a replacement because the specification moved
	// while the instance was down.
	r.ended(ctx, row, index, instances, event.InstanceExited)

	// A specification change is what makes an instance stale, and replacing it is
	// the only way to apply the change. The expected hash is the slot's own: an
	// instance carries the addresses it resolved, and two slots may legitimately
	// carry different ones.
	if stale, why := r.staleSlot(ctx, row, index, instances); len(stale) > 0 {
		if *replaced {
			// Another slot was replaced this pass. This one is due and rolls on a
			// later pass, which is what keeps a change from taking every instance
			// down at once.
			return slices.ContainsFunc(instances, running), nil
		}

		*replaced = true

		r.logger.With("workload", row.Name, "instance", index, "version", row.Version).Debug("replacing stale instance")

		// Recorded here rather than where the staleness was found, so a slot that is
		// due but rolls on a later pass does not report a replacement that has not
		// happened.
		r.record(ctx, row.Name, why.reason, why.fields)

		if err := r.stopInstance(ctx, row, index); err != nil {
			return false, fmt.Errorf("failed to stop stale instance: %w", err)
		}

		return false, r.start(ctx, row, index, event.InstanceStarted)
	}

	if slices.ContainsFunc(instances, running) {
		// Something is up and current, so there is nothing to do. It has to have
		// stayed up to count as settled: a container that exits the moment it
		// starts is genuinely observed as running on its way through, and clearing
		// the backoff on sight of that would reset the pacing every cycle.
		if slices.ContainsFunc(instances, func(i driver.Instance) bool { return settled(i, r.now()) }) {
			r.settle(row.Name, index)
		}

		return true, nil
	}

	// An instance whose runs have all ended under a policy that asks for nothing
	// further is finished with. It is left exactly as it is, so the outcome stays
	// readable, and the stale check is what runs it again once the specification
	// changes.
	if policy := restartPolicy(row); retired(policy, instances) {
		r.settle(row.Name, index)

		return false, nil
	}

	if len(instances) == 0 {
		return false, r.attempt(ctx, row, index)
	}

	return false, r.restart(ctx, row, index, instances, event.InstanceStarted)
}

// countOf reads how many instances a stored workload asks for.
//
// A specification that cannot be decoded reads as one. It was validated before it
// was stored, so failing here means the two have diverged, and converging one
// instance is a better failure than converging none.
func countOf(row database.Workload) int {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil || spec.Count < 1 {
		return 1
	}

	return spec.Count
}

// startAll starts every slot of a workload, reporting the first failure.
func (r *Reconciler) startAll(ctx context.Context, row database.Workload, count int) error {
	for index := range count {
		if err := r.start(ctx, row, index, event.InstanceStarted); err != nil {
			return err
		}
	}

	return nil
}

// staleSlot returns the slot's instances running a specification other than its
// current one.
//
// The comparison is against the slot's expected hash, which folds in the addresses
// this instance resolves. When those cannot be resolved — the target is mid-delete,
// or its ports are mid-move — the slot is left alone rather than judged against a
// hash that could not be computed: replacing it would start something that cannot
// resolve its environment either.
//
// The second return value says which of the two staleness is, so the replacement
// that follows can record why it happened. It is meaningful only where instances
// are returned.
func (r *Reconciler) staleSlot(
	ctx context.Context,
	row database.Workload,
	index int,
	instances []driver.Instance,
) ([]driver.Instance, staleness) {
	if len(instances) == 0 {
		return nil, staleness{}
	}

	expected, err := r.slotHash(ctx, row, index)
	if err != nil {
		r.logger.With("workload", row.Name, "instance", index, "error", err).
			Debug("leaving an instance whose expected hash cannot be resolved")

		// Recorded every pass that cannot resolve, and coalesced into one row, so a
		// workload waiting on another says so rather than sitting still in silence.
		r.record(ctx, row.Name, event.ReferenceUnresolved, event.Fields{Error: err.Error()})

		return nil, staleness{}
	}

	var stale []driver.Instance

	for _, instance := range instances {
		if instance.SpecHash != expected {
			stale = append(stale, instance)
		}
	}

	if len(stale) > 0 {
		return stale, staleness{
			reason: event.HashMoved,
			fields: event.Fields{Instance: index, Hash: expected, Previous: stale[0].SpecHash},
		}
	}

	// The hash says nothing about the slot's own host ports, which live in rows
	// rather than in the specification for every slot but the first. An instance
	// publishing ports its rows no longer name is bound to an address nothing
	// records, which is the same staleness by another route.
	ports := r.slotPorts(row, index)
	if portsDrifted(instances, ports) {
		return slices.Clone(instances), staleness{
			reason: event.PortsDrifted,
			fields: event.Fields{Instance: index, Ports: hostPorts(ports)},
		}
	}

	return nil, staleness{}
}

// portsDrifted reports whether a running instance publishes ports other than the
// ones its slot's rows record.
//
// Only an instance that reports its ports is judged — the exec runtime reports
// none, and its ports cannot move independently of its specification anyway.
func portsDrifted(instances []driver.Instance, rows []database.Port) bool {
	if len(rows) == 0 {
		return false
	}

	for _, instance := range instances {
		if instance.State != driver.StateRunning || len(instance.Ports) == 0 {
			continue
		}

		for _, row := range rows {
			published := slices.ContainsFunc(instance.Ports, func(port driver.Port) bool {
				return port.Container == row.Container && port.Host == row.Host && port.Protocol == row.Protocol
			})

			if !published {
				return true
			}
		}
	}

	return false
}

// slotPorts returns the port rows one instance of a workload holds, as of the most
// recent pass to read them.
func (r *Reconciler) slotPorts(row database.Workload, index int) []database.Port {
	allocations := r.allocations.Load()
	if allocations == nil {
		return nil
	}

	return slices.DeleteFunc(slices.Clone((*allocations)[row.ID]), func(port database.Port) bool {
		return port.Instance != index
	})
}

// slotHash returns the hash one instance's work is expected to carry.
//
// A workload referencing nothing expects the stored hash on every instance. One
// that references other workloads folds the addresses this instance resolves into
// it, because each instance may land on a different instance of a target — so a
// target's port moving replaces exactly the instances that were reading it, found
// by comparison on the next pass rather than by anything remembering to tell them.
func (r *Reconciler) slotHash(ctx context.Context, row database.Workload, index int) (string, error) {
	// This runs per slot per pass, so the common case of a workload referencing
	// nothing must not cost a decode of its whole specification. A reference is
	// stored verbatim in the canonical JSON, so the marker is present exactly when
	// one exists — the same trick refresh uses for its signal key.
	if r.env == nil || !bytes.Contains(row.Spec, []byte("${workload:")) {
		return row.SpecHash, nil
	}

	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return "", err
	}

	addresses, err := r.env.Addresses(ctx, spec.Env, row.Name, index)
	if err != nil {
		return "", err
	}

	if len(addresses) == 0 {
		return row.SpecHash, nil
	}

	digest := sha256.New()
	digest.Write([]byte(row.SpecHash))

	for _, reference := range slices.Sorted(maps.Keys(addresses)) {
		fmt.Fprintf(digest, "\n%s=%s", reference, addresses[reference])
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// refresh rewrites the values a running workload mounts and signals it for each one
// that changed.
//
// This is the other half of what a mount naming a signal asks for. Such a value is
// deliberately absent from the specification's hash, so nothing about the workload
// moves when it changes and the stale check will never fire: the file on disk is
// compared against what takt holds, and the workload is told.
//
// The signal follows the write, so a workload told to reload always finds the new
// contents. A failure to signal is returned rather than swallowed: the file has moved
// and the workload has not been told, so the pass has to report that it did not finish
// what it started. The digest is only recorded once the file is written, so the next
// pass tries again.
func (r *Reconciler) refresh(ctx context.Context, row database.Workload) error {
	if r.mounts == nil {
		return nil
	}

	// This runs for every workload that is up, on every pass, so the common case of a
	// workload that mounts nothing must not cost a decode of its whole specification.
	// No workload can want a refresh without naming a signal, and the stored bytes are
	// canonical JSON, so the key is present verbatim when one does.
	if !bytes.Contains(row.Spec, []byte(`"signal"`)) {
		return nil
	}

	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		// Validated before it was stored, so this means the specification and the rules
		// have diverged. Nothing about the mounts can be read, and the workload is left
		// running rather than being disturbed on the strength of a spec nothing could
		// read.
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	refreshed, err := r.mounts.Refresh(ctx, row.Name, row.ID, row.Version, spec)
	if err != nil {
		return fmt.Errorf("failed to refresh mounted values: %w", err)
	}

	if len(refreshed) == 0 {
		return nil
	}

	d, ok := r.driverFor(row)
	if !ok {
		// Nothing runs this runtime, which converge has already reported. The files are
		// written either way, so whatever eventually runs the workload reads the
		// current value.
		return nil
	}

	// One signal per distinct signal named, however many mounts changed. A workload
	// that mounts three secrets and rotates all of them wants to be told to reload, not
	// told three times.
	for _, signal := range slices.Sorted(maps.Keys(signalsOf(refreshed))) {
		if err = d.Signal(ctx, row.ID, row.Name, signal); err != nil {
			return fmt.Errorf("failed to signal workload on the %s runtime: %w", d.Name(), err)
		}

		r.logger.With("workload", row.Name, "signal", signal).Info("signalled a workload whose mounted values changed")
	}

	// One event per value rather than one per signal, which is the opposite grain
	// to the signals above. A signal is something the workload receives, so sending
	// it twice would be wrong. An event answers which value moved, and a workload
	// that rotated three secrets moved three.
	for _, refresh := range refreshed {
		r.record(ctx, row.Name, event.MountsRefreshed, event.Fields{
			Name:   refresh.Reference.String(),
			Signal: string(refresh.Signal),
		})
	}

	return nil
}

// signalsOf returns the set of signals a batch of refreshed mounts asks for.
func signalsOf(refreshed []mount.Refresh) map[string]struct{} {
	signals := make(map[string]struct{}, len(refreshed))
	for _, refresh := range refreshed {
		signals[string(refresh.Signal)] = struct{}{}
	}

	return signals
}

// restartPolicy reads what a stored workload asks for when its instance ends.
//
// A specification that cannot be decoded falls back to the default. It was validated
// before it was stored, so failing here means the two have diverged, and continuing to
// restart a workload is a better failure than retiring it on the strength of a spec
// nothing could read.
func restartPolicy(row database.Workload) *manifest.Restart {
	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return &manifest.Restart{Policy: manifest.RestartAlways, Delay: manifest.DefaultRestartDelay}
	}

	return spec.Restart
}

// hostPorts reads the host side of a set of allocations, which is the half an
// event names because it is the half an operator reaches the workload on.
func hostPorts(rows []database.Port) []int {
	ports := make([]int, 0, len(rows))
	for _, row := range rows {
		ports = append(ports, row.Host)
	}

	return ports
}

// retired reports whether every ended instance is one the policy leaves alone, and so
// whether the workload is finished with rather than waiting to be restarted.
//
// This asks only whether to act. How the workload ended is a separate question the
// caller answers from the exit code, because a workload that will not be restarted
// still has to say whether it succeeded.
//
// A workload with nothing observed at all is not retired. It has yet to run, and
// treating an empty driver as a finished job would mean a workload never started.
// Nor is one with an instance still up: the policy describes what happens when
// work ends, and that work has not.
func retired(restart *manifest.Restart, instances []driver.Instance) bool {
	if len(instances) == 0 {
		return false
	}

	for _, instance := range instances {
		if running(instance) || restart.Policy.Restarts(failureOf(instance)) {
			return false
		}
	}

	return true
}

// failureOf reports the exit code a restart policy judges an instance by.
//
// An instance the checker failed is still running, so it carries the exit code of a
// process that never exited, and a policy reading the code alone would see a clean
// exit and retire it. The state says what happened: a failed instance is a failure
// whatever number it carries.
func failureOf(instance driver.Instance) int {
	if failed(instance) && instance.ExitCode == 0 {
		return 1
	}

	return instance.ExitCode
}

// failureCodeOf reports the first exit code among the instances that a restart
// policy would read as a failure, or zero when there is none.
func failureCodeOf(instances []driver.Instance) int {
	for _, instance := range instances {
		if code := failureOf(instance); code != 0 {
			return code
		}
	}

	return 0
}

// running reports whether an instance counts as up for the purpose of deciding
// whether the workload needs anything done to it. Pending counts: a container still
// starting must not be replaced for not having started yet.
func running(instance driver.Instance) bool {
	return instance.State == driver.StateRunning || instance.State == driver.StatePending
}

func failed(instance driver.Instance) bool {
	return instance.State == driver.StateFailed
}

func terminating(instance driver.Instance) bool {
	return instance.State == driver.StateTerminating
}

func exitCodeOf(instances []driver.Instance) int {
	for _, instance := range instances {
		if instance.ExitCode != 0 {
			return instance.ExitCode
		}
	}

	return 0
}
