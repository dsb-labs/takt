// Package reconciler provides the control loop that drives the running state of
// the node towards the desired state held in the database.
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/driver/docker"
)

type (
	// The Driver interface describes the runtime operations the reconciler uses to
	// converge a workload onto its desired state.
	Driver interface {
		// Start should run the given workload, returning the driver's handle for it.
		Start(ctx context.Context, w docker.Workload) (string, error)
		// Stop should stop and discard everything the driver runs for the named workload.
		Stop(ctx context.Context, workload string) error
		// Observe should report every instance the driver is currently running.
		Observe(ctx context.Context) ([]driver.Instance, error)
		// Watch should report changes to the driver's instances so that the
		// reconciler can converge sooner than its next scheduled pass.
		Watch(ctx context.Context) (<-chan driver.Event, error)
	}

	// The WorkloadRepository interface describes the persistence operations the
	// reconciler uses to read desired state.
	WorkloadRepository interface {
		// List should return every stored workload.
		List(ctx context.Context) ([]database.Workload, error)
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
		logger    *slog.Logger
		driver    Driver
		workloads WorkloadRepository
		interval  time.Duration
		backoff   map[string]backoff
		nudge     chan struct{}
	}

	// The Config type contains fields used to construct a Reconciler.
	Config struct {
		// The logger used for reconciliation events.
		Logger *slog.Logger
		// The driver that runs workloads.
		Driver Driver
		// The repository holding desired state.
		Workloads WorkloadRepository
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
	// The delay before the first restart of a failed instance, doubled on each
	// consecutive failure up to maxBackoff.
	baseBackoff = time.Second
	// The ceiling on restart backoff, so a persistently broken workload is still
	// retried periodically.
	maxBackoff = 2 * time.Minute
)

// New returns a Reconciler that converges the driver in config onto the desired
// state in its repository.
func New(config Config) *Reconciler {
	return &Reconciler{
		logger:    config.Logger.With("component", "reconciler"),
		driver:    config.Driver,
		workloads: config.Workloads,
		interval:  config.Interval,
		backoff:   make(map[string]backoff),
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

	instances, err := r.driver.Observe(ctx)
	if err != nil {
		r.logger.With("error", err).Error("failed to observe driver instances")
		return
	}

	observed := make(map[string][]driver.Instance, len(instances))
	for _, instance := range instances {
		observed[instance.Workload] = append(observed[instance.Workload], instance)
	}

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

		if err = r.driver.Stop(ctx, workload); err != nil {
			r.logger.With("workload", workload, "error", err).Error("failed to stop orphaned workload")
		}
	}
}

// converge brings a single workload's running state into line with its desired
// state.
func (r *Reconciler) converge(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	// Only container workloads can run today. A workload naming any other runtime
	// is stored but left alone, so it starts working when its driver arrives
	// rather than being reported as broken.
	if api.Runtime(row.Runtime) != api.Container {
		return nil
	}

	// A specification change is what makes an instance stale, and replacing it is
	// the only way to apply the change: docker cannot mutate most of a container's
	// configuration in place.
	if stale := staleInstances(row, instances); len(stale) > 0 {
		r.logger.With("workload", row.Name, "version", row.Version).Debug("replacing stale workload")

		if err := r.driver.Stop(ctx, row.Name); err != nil {
			return fmt.Errorf("failed to stop stale workload: %w", err)
		}

		return r.start(ctx, row)
	}

	if slices.ContainsFunc(instances, running) {
		// Something is up and current, so the workload has converged. Clear any
		// backoff so a workload that failed in the past starts from a clean slate
		// the next time it does.
		delete(r.backoff, row.Name)

		return nil
	}

	if len(instances) == 0 {
		return r.start(ctx, row)
	}

	return r.restart(ctx, row, instances)
}

// restart brings back a workload whose instances have all stopped, pacing repeated
// failures with exponential backoff.
func (r *Reconciler) restart(ctx context.Context, row database.Workload, instances []driver.Instance) error {
	state := r.backoff[row.Name]
	if !state.next.IsZero() && time.Now().Before(state.next) {
		return nil
	}

	// The stopped instances have to be cleared before new work can take their
	// place: their container names are derived from the workload and version, so
	// a replacement would otherwise collide with the corpse.
	if err := r.driver.Stop(ctx, row.Name); err != nil {
		return fmt.Errorf("failed to clear stopped workload: %w", err)
	}

	if err := r.start(ctx, row); err != nil {
		return err
	}

	state.attempts++
	state.next = time.Now().Add(delay(state.attempts))
	r.backoff[row.Name] = state

	r.logger.With(
		"workload", row.Name,
		"attempts", state.attempts,
		"exit_code", exitCodeOf(instances),
	).Debug("restarted stopped workload")

	return nil
}

func (r *Reconciler) start(ctx context.Context, row database.Workload) error {
	w, err := docker.NewWorkload(row)
	if err != nil {
		return err
	}

	id, err := r.driver.Start(ctx, w)
	if err != nil {
		return fmt.Errorf("failed to start workload: %w", err)
	}

	r.logger.With("workload", row.Name, "instance", id, "version", row.Version).Info("workload started")

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

func running(instance driver.Instance) bool {
	return instance.State == driver.StateRunning || instance.State == driver.StatePending
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
