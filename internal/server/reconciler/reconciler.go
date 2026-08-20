// Package reconciler provides the control loop that drives the running state of
// the node towards the desired state held in the database.
package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The Driver interface describes the runtime operations the reconciler uses to
	// converge a workload onto its desired state.
	Driver interface {
		// Start should run the given workload, returning the driver's handle for it.
		Start(ctx context.Context, w driver.Workload) (string, error)
		// Stop should stop and discard everything the driver runs for the named workload.
		Stop(ctx context.Context, workload string) error
		// Observe should report every instance the driver is currently running.
		Observe(ctx context.Context) ([]driver.Instance, error)
		// Watch should report changes to the driver's instances so that the
		// reconciler can converge sooner than its next scheduled pass.
		Watch(ctx context.Context) (<-chan driver.Event, error)
	}

	// The WorkloadRepository interface describes the persistence operations the
	// reconciler uses to read desired state and to finish a deletion.
	WorkloadRepository interface {
		// List should return every stored workload.
		List(ctx context.Context, queries ...database.Query) ([]database.Workload, error)
		// Delete should remove the workload with the given name, which the
		// reconciler calls once the driver reports its work is gone.
		Delete(ctx context.Context, name string) error
	}

	// The PortRepository interface describes the port lookup the reconciler uses to
	// resolve the address a workload's health check should probe.
	PortRepository interface {
		// ListAll should return the ports allocated to every workload, keyed by
		// workload identifier.
		ListAll(ctx context.Context) (map[string][]database.Port, error)
	}

	// The Checker interface describes how the reconciler registers and reads what
	// orca established about a workload's health.
	Checker interface {
		// Set should register the check for a workload, replacing any it already had.
		Set(workload string, check health.Check)
		// Forget should drop the check for a workload that no longer exists.
		Forget(workload string)
		// Result should return the most recent outcome for a workload, reporting
		// false when it has no check registered.
		Result(workload string) (health.Result, bool)
	}

	// The Reconciler type drives the running state of the node towards the desired
	// state held in the repository.
	//
	// Reconciliation is level-triggered: every pass reads the full desired state,
	// asks the driver what is actually running, and acts on the difference. Nothing
	// is remembered between passes except restart backoff, so a missed event, a
	// failed pass, or a server restart all recover on the next pass rather than
	// leaving the node permanently wrong.
	Reconciler struct {
		logger     *slog.Logger
		driver     Driver
		workloads  WorkloadRepository
		ports      PortRepository
		checker    Checker
		reallocate func(ctx context.Context, workload string) (bool, error)
		interval   time.Duration
		backoff    map[string]backoff
		nudge      chan struct{}
	}

	// The Config type contains fields used to construct a Reconciler.
	Config struct {
		// The logger used for reconciliation events.
		Logger *slog.Logger
		// The driver that runs workloads.
		Driver Driver
		// The repository holding desired state.
		Workloads WorkloadRepository
		// The repository holding port allocations, used to resolve the address a
		// health check probes. May be nil, in which case no checks are registered.
		Ports PortRepository
		// Reports what orca's own health checks established. May be nil, in which
		// case only the state the driver reports is acted on.
		Checker Checker
		// Called to abandon the host ports orca chose for a workload when it fails
		// to start, reporting whether anything changed. May be nil, in which case
		// ports are never reallocated.
		Reallocate func(ctx context.Context, workload string) (bool, error)
		// How often a full reconciliation pass runs regardless of events.
		Interval time.Duration
	}

	// The backoff type paces restarts of a workload that keeps failing, so that a
	// container crashing in a loop doesn't spin the reconciler or the daemon.
	backoff struct {
		// The number of consecutive restarts attempted.
		attempts int
		// The earliest time the next restart may be attempted.
		next time.Time
	}
)

const (
	// How long a pass will wait on the driver before giving up on it.
	//
	// A pass is serial across workloads, so a driver that never answers doesn't just
	// delay one workload — it stops every other workload converging behind it, and a
	// docker daemon that has wedged will do exactly that. The deadline is generous
	// enough for a slow daemon under load and short enough that a stuck one costs a
	// pass rather than the node.
	//
	// Starting a workload gets its own, longer deadline: it may have to pull an image
	// first, which is legitimately slow and not a sign that anything is wrong.
	driverTimeout = 30 * time.Second
	// How long starting a workload may take, including pulling its image.
	startTimeout = 10 * time.Minute
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

// New returns a Reconciler that converges the driver in config onto the desired
// state in its repository.https://github.com/octplane
func New(config Config) *Reconciler {
	return &Reconciler{
		logger:     config.Logger.With("component", "reconciler"),
		driver:     config.Driver,
		workloads:  config.Workloads,
		ports:      config.Ports,
		checker:    config.Checker,
		reallocate: config.Reallocate,
		interval:   config.Interval,
		backoff:    make(map[string]backoff),
		// Buffered so that a caller signalling a change never blocks: a pass is
		// already pending, which is all the signal conveys.
		nudge: make(chan struct{}, 1),
	}
}

// Notify asks for a reconciliation pass to run as soon as possible, and is how a
// caller reports that desired state has changed. It never blocks.
func (r *Reconciler) Notify() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is cancelled, returning nil on a clean shutdown.
//
// Passes run on a ticker, when the driver reports a change, and when Notify is
// called. Passes never overlap: each is driven from this one goroutine, so a burst
// of events coalesces into a single pass rather than racing.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	events, err := r.driver.Watch(ctx)
	if err != nil {
		return fmt.Errorf("failed to watch driver: %w", err)
	}

	// Converge once at startup so that a workload applied before the server was
	// restarted is running again without waiting for the first tick.
	r.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.nudge:
			r.reconcile(ctx)
		case event, ok := <-events:
			if !ok {
				// The driver's event stream ended. The ticker still guarantees
				// convergence, so carry on with reduced responsiveness rather
				// than bringing the server down.
				r.logger.Debug("driver event stream closed, relying on periodic reconciliation")
				events = nil

				continue
			}

			r.logger.With("workload", event.Workload).Debug("reconciling after driver event")
			r.reconcile(ctx)
		}
	}
}

// reconcile runs a single pass. Errors affecting one workload are logged and the
// pass continues, so one broken workload can't stop the others converging.
func (r *Reconciler) reconcile(ctx context.Context) {
	rows, err := r.workloads.List(ctx)
	if err != nil {
		r.logger.With("error", err).Error("failed to list workloads")
		return
	}

	observeCtx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	instances, err := r.driver.Observe(observeCtx)
	if err != nil {
		r.logger.With("error", err).Error("failed to observe driver instances")
		return
	}

	observed := make(map[string][]driver.Instance, len(instances))
	for _, instance := range instances {
		observed[instance.Workload] = append(observed[instance.Workload], r.checked(instance))
	}

	r.register(ctx, rows, observed)

	desired := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		desired[row.Name] = struct{}{}

		if err = r.converge(ctx, row, observed[row.Name]); err != nil {
			r.logger.With("workload", row.Name, "error", err).Error("failed to reconcile workload")
		}
	}

	// Anything the driver is running that nothing asked for is an orphan — most
	// often the remnant of a workload deleted while the server was down.
	for workload := range observed {
		if _, ok := desired[workload]; ok {
			continue
		}

		r.logger.With("workload", workload).Debug("stopping orphaned workload")

		if err = r.stop(ctx, workload); err != nil {
			r.logger.With("workload", workload, "error", err).Error("failed to stop orphaned workload")
		}
	}
}

// converge brings a single workload's running state into line with its desired
// state.
func (r *Reconciler) converge(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	// A workload marked for deletion is torn down here rather than by whoever
	// asked, so that one component is responsible for touching the runtime and the
	// desired state survives until the work described by it is actually gone.
	if !row.DeletedAt.IsZero() {
		return r.teardown(ctx, row, instances)
	}

	// Only container workloads can run today. A workload naming any other runtime
	// is stored but left alone, so it starts working when its driver arrives
	// rather than being reported as broken.
	if api.Runtime(row.Runtime) != api.Container {
		return nil
	}

	// An instance on its way out is mid-teardown from an earlier pass. Acting now
	// would mean stopping what is already stopping, or starting a replacement whose
	// name the departing container still holds, so the pass leaves the workload
	// alone and picks it up once the runtime has finished.
	if slices.ContainsFunc(instances, terminating) {
		r.logger.With("workload", row.Name).Debug("waiting for workload to finish terminating")

		return nil
	}

	// A specification change is what makes an instance stale, and replacing it is
	// the only way to apply the change: docker cannot mutate most of a container's
	// configuration in place.
	if stale := staleInstances(row, instances); len(stale) > 0 {
		r.logger.With("workload", row.Name, "version", row.Version).Debug("replacing stale workload")

		if err := r.stop(ctx, row.Name); err != nil {
			return fmt.Errorf("failed to stop stale workload: %w", err)
		}

		return r.start(ctx, row)
	}

	if slices.ContainsFunc(instances, running) {
		// Something is up and current, so there is nothing to do. Whether the
		// workload has converged is a separate question: a container that exits the
		// moment it starts is genuinely observed as running on its way through, so
		// clearing the backoff on sight of that reset the pacing every cycle and let
		// such a workload loop at five containers a second indefinitely. It has to
		// have stayed up to count as settled.
		if slices.ContainsFunc(instances, settled) {
			delete(r.backoff, row.Name)
		}

		return nil
	}

	// A workload whose instances have all ended under a policy that asks for nothing
	// further is finished with. It is left exactly as it is: the containers stay so
	// that the outcome remains readable, and the stale check above is what runs the
	// workload again once its specification changes.
	//
	// Whether it succeeded is not decided here. A clean exit reads as completed and a
	// dirty one stays failed, which the service derives from the exit code the driver
	// reported — so `never` retires a failed workload without calling it a success.
	if policy := restartPolicy(row); retired(policy, instances) {
		delete(r.backoff, row.Name)

		return nil
	}

	if len(instances) == 0 {
		return r.attempt(ctx, row)
	}

	return r.restart(ctx, row, instances)
}

// register keeps the checker in step with the desired state, so that every workload
// declaring a check has one and no workload that has gone still does.
//
// This belongs to the pass rather than to whoever applies a workload: the reconciler
// is what runs continuously, so a server that restarts resumes checking the workloads
// it adopts without waiting for anything to be applied or read again.
func (r *Reconciler) register(ctx context.Context, rows []database.Workload, observed map[string][]driver.Instance) {
	if r.checker == nil || r.ports == nil {
		return
	}

	allocations, err := r.ports.ListAll(ctx)
	if err != nil {
		r.logger.With("error", err).Error("failed to read workload ports")
		return
	}

	for _, row := range rows {
		check, ok, err := healthCheck(row, allocations[row.ID])
		switch {
		case err != nil:
			// The specification was validated before it was stored, so a check that
			// cannot be resolved now means the two have diverged rather than that the
			// operator made a mistake.
			r.logger.With("workload", row.Name, "error", err).Error("failed to resolve health check")
		case ok && row.DeletedAt.IsZero() && !retired(restartPolicy(row), observed[row.Name]):
			r.checker.Set(row.Name, check)
		default:
			// The workload declares no check, or is on its way out, or has ended and
			// will not be restarted. None is worth probing, and probing the last would
			// report a finished workload as unhealthy for no longer answering.
			r.checker.Forget(row.Name)
		}
	}
}

// restartPolicy reads what a stored workload asks for when its instance ends.
//
// A specification that cannot be decoded falls back to the default. It was validated
// before it was stored, so failing here means the two have diverged, and continuing to
// restart a workload is a better failure than retiring it on the strength of a spec
// nothing could read.
func restartPolicy(row database.Workload) manifest.RestartPolicy {
	var spec api.WorkloadSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return manifest.RestartAlways
	}

	return manifest.NewSpec(spec).Restart
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
func retired(policy manifest.RestartPolicy, instances []driver.Instance) bool {
	if len(instances) == 0 {
		return false
	}

	for _, instance := range instances {
		if policy.Restarts(instance.ExitCode) {
			return false
		}
	}

	return true
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

// checked folds what orca's health check established into an instance's state, so
// that a workload the driver reports as running but which cannot serve converges
// instead of being left alone.
//
// A failing check makes the instance failed, which routes it into the same paced
// restart a crashed container takes: the reaction to "not working" is the same
// whether the process died or merely stopped answering. A check that has not passed
// yet makes it pending, which the pass treats as up — a workload still starting
// must not be replaced for not having answered yet.
func (r *Reconciler) checked(instance driver.Instance) driver.Instance {
	if r.checker == nil || instance.State != driver.StateRunning {
		return instance
	}

	result, ok := r.checker.Result(instance.Workload)
	if !ok {
		return instance
	}

	switch result.Status {
	case health.StatusUnhealthy:
		instance.State = driver.StateFailed
	case health.StatusStarting:
		instance.State = driver.StatePending
	}

	return instance
}

// teardown removes a workload that has been marked for deletion, and its desired
// state once the driver reports nothing is left.
//
// The row is the last thing to go. While it exists the workload reads as
// terminating, so the teardown is observable; once the driver is empty there is
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

		if err := r.stop(ctx, row.Name); err != nil {
			return fmt.Errorf("failed to stop deleted workload: %w", err)
		}

		// The stop may not have taken effect yet, so the row is left for the next
		// pass to reap once the driver reports the work is gone.
		return nil
	}

	if err := r.workloads.Delete(ctx, row.Name); err != nil {
		return fmt.Errorf("failed to delete workload: %w", err)
	}

	// Backoff is keyed by workload and would otherwise outlive it, pacing the
	// restarts of a later workload that happens to reuse the name.
	delete(r.backoff, row.Name)

	r.logger.With("workload", row.Name).Info("workload deleted")

	return nil
}

// attempt starts a workload that has nothing running, pacing repeated failures with
// the same backoff a repeatedly-crashing workload gets.
//
// A workload can fail to start for reasons no amount of retrying will fix — an image
// that does not exist, a host port held by something outside orca and no free port to
// move to. Without pacing, such a workload is retried on every pass and every driver
// event, which was measured filling the log at over a thousand errors in four
// minutes while achieving nothing.
func (r *Reconciler) attempt(ctx context.Context, row database.Workload) error {
	if r.waiting(row.Name) {
		return nil
	}

	if err := r.start(ctx, row); err != nil {
		r.hold(row.Name)

		return err
	}

	delete(r.backoff, row.Name)

	return nil
}

// restart brings back a workload whose instances have all stopped, pacing repeated
// failures with exponential backoff.
func (r *Reconciler) restart(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	if r.waiting(row.Name) {
		return nil
	}

	// The stopped instances have to be cleared before new work can take their
	// place: their container names are derived from the workload and version, so
	// a replacement would otherwise collide with the corpse.
	if err := r.stop(ctx, row.Name); err != nil {
		return fmt.Errorf("failed to clear stopped workload: %w", err)
	}

	if err := r.start(ctx, row); err != nil {
		r.hold(row.Name)

		return err
	}

	state := r.hold(row.Name)

	r.logger.With(
		"workload", row.Name,
		"attempts", state.attempts,
		"exit_code", exitCodeOf(instances),
	).Debug("restarted stopped workload")

	return nil
}

// waiting reports whether a workload is still inside its backoff window.
func (r *Reconciler) waiting(workload string) bool {
	state := r.backoff[workload]

	return !state.next.IsZero() && time.Now().Before(state.next)
}

// hold records another attempt against a workload and pushes out the earliest time
// the next one may happen.
func (r *Reconciler) hold(workload string) backoff {
	state := r.backoff[workload]

	state.attempts++
	state.next = time.Now().Add(delay(state.attempts))
	r.backoff[workload] = state

	return state
}

func (r *Reconciler) start(ctx context.Context, row database.Workload) error {
	w, err := driver.NewWorkload(row)
	if err != nil {
		return err
	}

	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	id, err := r.driver.Start(startCtx, w)
	if err != nil {
		// A workload that cannot start may be sitting on a host port something
		// outside orca has taken, which nothing orca does will free. Rather than
		// try to recognise that specific failure — docker reports it as an
		// untyped error whose wording is not part of any contract — any failure
		// gives up the ports orca chose for itself. Ports the specification
		// pinned are left alone: they were asked for, so moving them would be
		// overriding a decision rather than revising a guess.
		r.abandonPorts(ctx, row)

		return fmt.Errorf("failed to start workload: %w", err)
	}

	r.logger.With("workload", row.Name, "instance", id, "version", row.Version).Info("workload started")

	return nil
}

// abandonPorts gives up the host ports orca chose for a workload, so that the next
// pass tries different ones. Failures are logged rather than returned: the caller is
// already reporting why the workload didn't start, and a workload that keeps its
// ports is no worse off than before.
func (r *Reconciler) abandonPorts(ctx context.Context, row database.Workload) {
	if r.reallocate == nil {
		return
	}

	changed, err := r.reallocate(ctx, row.Name)
	switch {
	case err != nil:
		r.logger.With("workload", row.Name, "error", err).Error("failed to reallocate workload ports")
	case changed:
		r.logger.With("workload", row.Name).Info("reallocated host ports after a failed start")
	}
}

// stop asks the driver to stop a workload, bounded so that a daemon which never
// answers costs one pass rather than blocking every workload behind it.
func (r *Reconciler) stop(ctx context.Context, workload string) error {
	ctx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	if err := r.driver.Stop(ctx, workload); err != nil {
		return err
	}

	// The check history describes work that no longer exists. Keeping it would
	// condemn the replacement for failures the departed container produced, and deny
	// it the start period a newly started workload is owed — so a workload replaced
	// for being unhealthy could never demonstrate that it had recovered.
	if r.checker != nil {
		r.checker.Forget(workload)
	}

	return nil
}

// staleInstances returns the instances running a specification other than the
// workload's current one.
func staleInstances(row database.Workload, instances []driver.Instance) []driver.Instance {
	var stale []driver.Instance
	for _, instance := range instances {
		if instance.SpecHash != row.SpecHash {
			stale = append(stale, instance)
		}
	}

	return stale
}

// running reports whether an instance counts as up for the purpose of deciding
// whether the workload needs anything done to it. Pending counts: a container still
// starting must not be replaced for not having started yet.
func running(instance driver.Instance) bool {
	return instance.State == driver.StateRunning || instance.State == driver.StatePending
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
func settled(instance driver.Instance) bool {
	if instance.State != driver.StateRunning {
		return false
	}

	return instance.StartedAt.IsZero() || time.Since(instance.StartedAt) >= settlePeriod
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

// delay returns how long to wait before the given restart attempt, doubling with
// each consecutive failure up to maxBackoff.
func delay(attempts int) time.Duration {
	d := baseBackoff << min(attempts, 8)
	if d > maxBackoff {
		return maxBackoff
	}

	return d
}
