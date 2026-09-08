// Package reconciler provides the control loop that drives the running state of
// the node towards the desired state held in the database.
package reconciler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/internal/server/mount"
	"github.com/dsb-labs/takt/internal/server/state"
	"github.com/dsb-labs/takt/internal/server/telemetry"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The name this package's telemetry is recorded under, which describes the code
// declaring it rather than whatever assembles the server.
const scope = "github.com/dsb-labs/takt/internal/server/reconciler"

type (
	// The Driver interface describes the runtime operations the reconciler uses to
	// converge a workload onto its desired state.
	Driver interface {
		// Name should return the name the driver is known by, which is what a
		// workload's runtime is matched against to decide what runs it.
		Name() string
		// Start should run the given workload, returning the driver's handle for it.
		Start(ctx context.Context, w driver.Workload) (string, error)
		// Stop should stop everything the driver runs for a workload, keeping the
		// instance it most recently stopped so that its output can still be read.
		//
		// The identifier is empty for an orphan, which by definition has no stored
		// workload to take one from. A driver that keys its own storage on the
		// identifier has to find such a workload by its name instead.
		Stop(ctx context.Context, id, workload string) error
		// Discard should stop everything the driver runs for a workload and remove all
		// of it, including whatever Stop kept for its output.
		//
		// This is what a workload nobody wants any more takes. A kept instance exists so
		// that an operator can read why an attempt failed, and a deleted workload has no
		// such reader — one left behind is work the orphan sweep would find forever.
		Discard(ctx context.Context, id, workload string) error
		// StopInstance should stop what the driver runs for one instance of a
		// workload, retaining its most recent output exactly as Stop does for
		// every instance. This is what replacing a single instance takes.
		StopInstance(ctx context.Context, id, workload string, instance int) error
		// DiscardInstance should stop what the driver runs for one instance of a
		// workload and remove all of it, including what was retained for it. This
		// is what removing an instance takes when a workload's count shrinks.
		DiscardInstance(ctx context.Context, id, workload string, instance int) error
		// Signal should send the named signal to everything the driver runs for a
		// workload, so that a workload mounting a value can be told the value
		// changed rather than being replaced.
		//
		// A driver with nothing running for the workload should do nothing rather
		// than fail. There is nothing to reload, and the reconciler asks only the
		// driver that runs the workload anyway.
		Signal(ctx context.Context, id, workload, signal string) error
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

	// The Resolver interface describes how the reconciler turns the references in a
	// workload's environment into the values it is started with.
	//
	// Resolution happens here, as late as it can, so that a secret's plaintext
	// exists only for as long as it takes to start the work that needs it. Nothing
	// the reconciler persists holds one.
	Resolver interface {
		// Resolve should return env with every reference replaced by the value it
		// names, reporting an error when one cannot be resolved.
		Resolve(ctx context.Context, env map[string]string, reader string, readerInstance int) (map[string]string, error)
		// Addresses should return the resolved address of every workload
		// reference in env, keyed by the reference as written. This is what an
		// instance's expected hash covers: each instance may resolve a reference
		// to a different address, so staleness has to be judged against its own.
		Addresses(ctx context.Context, env map[string]string, reader string, readerInstance int) (map[string]string, error)
	}

	// The Mounts interface describes how the reconciler turns the secrets and
	// variables a workload mounts into files on the host.
	//
	// Delivery happens here, as late as it can, for the same reason resolution does: a
	// value's plaintext exists on disk only for as long as the workload reading it.
	Mounts interface {
		// Deliver should write a file for every value the specification mounts and
		// return them as mounts the driver can honour.
		Deliver(ctx context.Context, id string, version int, spec manifest.Spec) ([]driver.Volume, error)
		// Refresh should rewrite the mounted values that have changed since they were
		// delivered, reporting the signal each affected workload asked for.
		Refresh(ctx context.Context, name, id string, version int, spec manifest.Spec) ([]mount.Refresh, error)
		// Forget should remove the files written for a workload, once nothing is
		// running for it.
		Forget(id string) error
		// Reclaim should remove every version of a workload's files except the one
		// named.
		Reclaim(id string, keep int) error
		// Prune should remove the files written for workloads other than those named.
		Prune(keep []string) error
	}

	// The Checker interface describes how the reconciler registers and reads what
	// takt established about a workload's health.
	Checker interface {
		// Set should register the check for one instance of a workload, replacing
		// any it already had.
		Set(workload string, instance int, check health.Check)
		// Forget should drop every check for a workload that no longer exists.
		Forget(workload string)
		// ForgetInstance should drop one instance's check, once the instance is
		// gone while its workload stays.
		ForgetInstance(workload string, instance int)
		// Result should return the most recent outcome for one instance of a
		// workload, reporting false when it has no check registered.
		Result(workload string, instance int) (health.Result, bool)
	}

	// The Reconciler type drives the running state of the node towards the desired
	// state held in the repository.
	//
	// Reconciliation is level-triggered: every pass reads the full desired state,
	// asks the driver what is actually running, and acts on the difference. Nothing
	// is remembered between passes except restart backoff, pending restart
	// requests, the last converge error and how each driver last answered, so a
	// missed event, a failed pass, or a server restart all recover on the next
	// pass rather than leaving the node permanently wrong.
	Reconciler struct {
		logger      *slog.Logger
		drivers     map[string]Driver
		workloads   WorkloadRepository
		ports       PortRepository
		env         Resolver
		mounts      Mounts
		checker     Checker
		bind        string
		reallocate  func(ctx context.Context, workload string, instance int) (bool, error)
		now         func() time.Time
		interval    time.Duration
		nudge       chan struct{}
		tracer      trace.Tracer
		instruments instruments

		// The port allocations the current pass converges against, written once as
		// a pass begins and read by the converges it spawns. Unguarded because
		// passes never overlap: the loop's goroutine writes it before anything
		// concurrent reads it.
		allocations map[string][]database.Port

		// Guards backoff, lastError and restarts, which are the only state a pass
		// carries between workloads and so the only things converging them
		// concurrently can contend on.
		mux sync.Mutex
		// How long to wait before restarting each instance that keeps failing.
		// Keyed per instance, so one instance crashing does not pace the others.
		backoff map[slot]backoff
		// The workloads whose instances an operator asked to have replaced,
		// consumed by the next pass over each. In memory rather than stored,
		// because a request the server loses can simply be made again.
		restarts map[string]struct{}
		// Why the last converge pass over each workload failed. In memory rather
		// than stored, because a converge error has no live source to re-derive
		// from once the pass has ended: after a restart the next pass either fails
		// again and repopulates it, or succeeds, and either answer is current.
		lastError map[string]lastError
		// Counts completed passes, so that a caller can tell a pass has finished
		// rather than inferring it from something a pass happens to do first.
		passes atomic.Uint64

		// Guards observations, separately from mux so a caller polling readiness
		// never contends with a pass's backoff bookkeeping.
		obsMux sync.Mutex
		// How the most recent observation of each driver ended, keyed by runtime
		// name. Seeded with a zero value per driver so a driver that has never
		// answered reads as not ready rather than as absent.
		observations map[string]Observation
	}

	// The Config type contains fields used to construct a Reconciler.
	Config struct {
		// The logger used for reconciliation events.
		Logger *slog.Logger
		// The drivers that run workloads, keyed by the name each one declares. A
		// workload whose runtime names no driver here is left alone.
		Drivers map[string]Driver
		// The repository holding desired state.
		Workloads WorkloadRepository
		// The repository holding port allocations, used to resolve the address a
		// health check probes. May be nil, in which case no checks are registered.
		Ports PortRepository
		// Resolves the secrets and variables a workload reads, as it starts. May be
		// nil, in which case a workload's environment is passed to its driver as
		// stored — so a reference of either kind reaches the workload as the text it
		// is written as.
		Env Resolver
		// Materialises the secrets and variables a workload mounts. May be nil, in
		// which case a workload mounting either is started without the files it asked
		// for — so one is only ever nil where no workload can mount anything, as in
		// tests.
		Mounts Mounts
		// Reports what takt's own health checks established. May be nil, in which
		// case only the state the driver reports is acted on.
		Checker Checker
		// The address a workload's host ports are published on, which is where a
		// health check is performed. Empty probes loopback.
		Bind string
		// Called to abandon the host ports takt chose for one instance of a
		// workload when it fails to start, reporting whether anything changed. May
		// be nil, in which case ports are never reallocated.
		Reallocate func(ctx context.Context, workload string, instance int) (bool, error)
		// How often a full reconciliation pass runs regardless of events.
		Interval time.Duration
		// Reports the current time, which every timing decision a pass makes reads
		// from. May be nil, in which case the wall clock is used.
		//
		// A test names the times it wants rather than waiting for them, which matters
		// most for a schedule: the finest cron expression names one time a minute.
		//
		// Durations recorded as metrics read the wall clock regardless, so a test
		// that fakes the time does not record garbage.
		Now func() time.Time
		// The meter instruments are created from. May be nil, in which case
		// nothing is recorded.
		MeterProvider metric.MeterProvider
		// The tracer spans are created from. May be nil, in which case no spans
		// are recorded.
		TracerProvider trace.TracerProvider
	}

	// The Observation type records how the most recent attempt to observe one
	// driver ended, which is what the readiness endpoint reports.
	Observation struct {
		// When the driver was last asked. Zero when it has not been asked yet.
		At time.Time
		// What the driver answered. Empty when it succeeded.
		Error string
	}

	// The backoff type paces restarts of a workload that keeps failing, so that a
	// container crashing in a loop doesn't spin the reconciler or the daemon.
	backoff struct {
		// The number of consecutive restarts attempted.
		attempts int
		// The earliest time the next restart may be attempted.
		next time.Time
		// Whether the workload has been given up on. The decision repeats on
		// every pass over a workload that stays given up, and this is what lets
		// it count once.
		gaveUp bool
	}

	// The lastError type records why a converge pass over a workload failed, so
	// that a workload sitting pending can say what is stopping it.
	lastError struct {
		// What the failing pass reported.
		message string
		// When the failure was recorded.
		at time.Time
	}

	// The slot type identifies one instance of one workload, which is the grain
	// backoff and health are kept at.
	slot struct {
		workload string
		instance int
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
	// How long a pass triggered by a driver event waits to collect the events
	// behind it before it runs.
	//
	// A burst of events is one piece of news: a mass teardown emits an event per
	// container, and a pass observes the whole runtime anyway, so running a pass
	// per event puts a full observation behind each of them. Measured on a
	// thousand-workload teardown as over a thousand back-to-back passes. The
	// window trades that for half a second of latency on the first event, which
	// the reconcile interval dwarfs.
	coalesceWindow = 500 * time.Millisecond
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

// clock returns the function a reconciler reads the time from, defaulting to the wall
// clock so that only a test has to say anything about it.
func clock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}

	return now
}

// New returns a Reconciler that converges the drivers in config onto the desired
// state in its repository.
func New(config Config) *Reconciler {
	observations := make(map[string]Observation, len(config.Drivers))
	for name := range config.Drivers {
		observations[name] = Observation{}
	}

	return &Reconciler{
		logger:       config.Logger.With("component", "reconciler"),
		drivers:      config.Drivers,
		workloads:    config.Workloads,
		ports:        config.Ports,
		env:          config.Env,
		mounts:       config.Mounts,
		checker:      config.Checker,
		bind:         config.Bind,
		reallocate:   config.Reallocate,
		now:          clock(config.Now),
		interval:     config.Interval,
		backoff:      make(map[slot]backoff),
		restarts:     make(map[string]struct{}),
		lastError:    make(map[string]lastError),
		observations: observations,
		tracer:       telemetry.Tracer(config.TracerProvider, scope),
		instruments:  newInstruments(telemetry.Meter(config.MeterProvider, scope)),
		// Buffered so that a caller signalling a change never blocks: a pass is
		// already pending, which is all the signal conveys.
		nudge: make(chan struct{}, 1),
	}
}

// Restart records that a workload's instances should be replaced on the next
// pass over it. This is how an operator's restart request reaches the loop,
// since the reconciler is the only component that touches the runtime.
//
// Held in memory rather than stored. A request the server loses to a crash can
// simply be made again, where a stored one would lie in wait for whoever starts
// the server next.
func (r *Reconciler) Restart(workload string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	r.restarts[workload] = struct{}{}
}

// restartRequested consumes any pending restart request for a workload,
// reporting whether one was present.
//
// Consumed on sight rather than on success: a replacement that fails to start
// is retried and paced by the ordinary paths on later passes, so acting on the
// request again would repeat work those paths already own.
func (r *Reconciler) restartRequested(workload string) bool {
	r.mux.Lock()
	defer r.mux.Unlock()

	_, ok := r.restarts[workload]
	delete(r.restarts, workload)

	return ok
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
// called. Passes never overlap: each is driven from this one goroutine. A driver
// event does not run a pass alone — the events behind it are collected for a
// short window first, so a burst coalesces into one pass rather than queueing
// one each.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	events, err := r.watch(ctx)
	if err != nil {
		return err
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
			coalesce(ctx, events)
			r.reconcile(ctx)
		}
	}
}

// coalesce holds a pass back for the coalesce window, consuming the driver events
// that arrive in it. The pass that follows observes the whole runtime, so the
// discarded events tell it nothing it will not see for itself.
func coalesce(ctx context.Context, events <-chan driver.Event) {
	timer := time.NewTimer(coalesceWindow)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case _, ok := <-events:
			if !ok {
				return
			}
		}
	}
}

// Passes reports how many reconciliation passes have completed.
//
// A pass observes the runtimes before it acts, so nothing a pass does first says that
// it has finished — and workloads are converged concurrently, so the last of them may
// still be in flight when the loop moves on. This counts what a caller actually wants
// to know.
func (r *Reconciler) Passes() uint64 {
	return r.passes.Load()
}

// Observations reports how the most recent attempt to observe each driver ended,
// keyed by runtime name. A driver that has never been asked reports a zero
// Observation.
//
// This is what the readiness endpoint reads. It is recorded as a pass observes,
// so answering costs nothing and is at most one interval plus the driver timeout
// stale — a cached recent answer rather than a live round-trip per poll.
func (r *Reconciler) Observations() map[string]Observation {
	r.obsMux.Lock()
	defer r.obsMux.Unlock()

	return maps.Clone(r.observations)
}

// reconcile runs a single pass. Errors affecting one workload are logged and the
// pass continues, so one broken workload can't stop the others converging.
func (r *Reconciler) reconcile(ctx context.Context) {
	started := time.Now()
	outcome := telemetry.OutcomeOK

	ctx, span := r.tracer.Start(ctx, "reconcile")

	defer func() {
		// Always set, so a TraceQL filter on the attribute needs no special
		// case for the passes that went wrong.
		span.SetAttributes(attribute.String("takt.outcome", string(outcome)))

		if outcome != telemetry.OutcomeOK {
			span.SetStatus(codes.Error, string(outcome))
		}

		span.End()

		set := metric.WithAttributes(outcome.Attribute())
		r.instruments.passes.Add(ctx, 1, set)
		r.instruments.passDuration.Record(ctx, time.Since(started).Seconds(), set)

		r.passes.Add(1)
	}()

	rows, err := r.workloads.List(ctx)
	if err != nil {
		outcome = outcomeListFailed
		span.RecordError(err)
		r.logger.With("error", err).Error("failed to list workloads")

		return
	}

	span.SetAttributes(attribute.Int("takt.workloads", len(rows)))

	observeCtx, cancel := context.WithTimeout(ctx, driverTimeout)
	defer cancel()

	instances, err := r.observe(observeCtx)
	if err != nil {
		outcome = outcomeObserveFailed
		span.RecordError(err)
		r.logger.With("error", err).Error("failed to observe driver instances")

		return
	}

	span.SetAttributes(attribute.Int("takt.instances", len(instances)))

	// Two views of the same observation, because two questions are being asked of it.
	//
	// A driver keeps the instance it most recently stopped so that its output survives
	// the attempt that produced it. Such an instance has ended and nothing will restart
	// it, so anything deciding what to run has to leave it out: counted as an instance
	// it would read as a stale one to replace, as a failure to pace, or as work already
	// present that needs nothing done. Orphan detection is the opposite — a workload
	// deleted while the server was down leaves a retained instance, and one left out
	// here would never be reaped.
	observed := make(map[string][]driver.Instance, len(instances))
	held := make(map[string]struct{}, len(instances))

	// The instance indexes each workload holds anything under, retained included.
	// Scaling down has to see a slot whose only remnant is a retained corpse, or
	// the corpse outlives the count that removed it.
	indexes := make(map[string]map[int]struct{}, len(instances))

	for _, instance := range instances {
		held[instance.Workload] = struct{}{}

		if indexes[instance.Workload] == nil {
			indexes[instance.Workload] = make(map[int]struct{})
		}

		indexes[instance.Workload][instance.Index] = struct{}{}

		if instance.Retained {
			continue
		}

		observed[instance.Workload] = append(observed[instance.Workload], r.checked(instance))
	}

	// Read once per pass rather than per workload. Written before the converges
	// spawn, so they read it without contention.
	r.allocations = nil
	if r.ports != nil {
		if r.allocations, err = r.ports.ListAll(ctx); err != nil {
			outcome = outcomeObserveFailed
			r.logger.With("error", err).Error("failed to read workload ports")

			return
		}
	}

	r.measure(ctx, rows, observed)
	r.register(rows, observed)

	desired := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		desired[row.Name] = struct{}{}
	}

	r.convergeAll(ctx, rows, observed, indexes)

	// Anything a driver holds that nothing asked for is an orphan — most often the
	// remnant of a workload deleted while the server was down.
	for workload := range held {
		if _, ok := desired[workload]; ok {
			continue
		}

		r.logger.With("workload", workload).Debug("stopping orphaned workload")

		if err = r.stopOrphan(ctx, workload); err != nil {
			r.logger.With("workload", workload, "error", err).Error("failed to stop orphaned workload")
		}
	}

	r.prune(rows)
}

// prune removes the mounted values of workloads that no longer exist.
//
// A teardown removes them itself, so this is for the case that teardown cannot cover: a
// server that stopped between stopping the work and removing the row leaves files whose
// workload is gone, and a secret's plaintext should not sit on the disk waiting for
// something to notice.
//
// That is a condition a server recovers from rather than one that arises while it runs,
// so this reads the disk on the first pass and then only occasionally. Doing it every
// pass cost two directory reads per pass forever — on a host where nothing has ever been
// mounted, two failing syscalls — to answer a question whose answer only changes when a
// server stops at exactly the wrong moment.
//
// Failures are logged rather than returned. Nothing is worse off for the files
// remaining, and a later pass tries again.
func (r *Reconciler) prune(rows []database.Workload) {
	const every = 64

	if r.mounts == nil || r.passes.Load()%every != 0 {
		return
	}

	keep := make([]string, 0, len(rows))
	for _, row := range rows {
		keep = append(keep, row.ID)
	}

	if err := r.mounts.Prune(keep); err != nil {
		r.logger.With("error", err).Error("failed to remove the mounted values of workloads that no longer exist")
	}
}

// measure records the number of workloads in each state.
//
// Each state is derived by the same rules the API reports it under, so a
// dashboard and a workload listing never disagree. The instances are cloned
// before the restart policy is folded in, because the observed map is what the
// rest of the pass converges from.
func (r *Reconciler) measure(ctx context.Context, rows []database.Workload, observed map[string][]driver.Instance) {
	// Every state is recorded each pass, so a state nothing is in reads as zero
	// rather than holding whatever it last was.
	counts := make(map[state.Workload]int, len(state.Workloads))
	for _, row := range rows {
		policy := restartPolicy(row)

		instances := slices.Clone(observed[row.Name])
		for i := range instances {
			instances[i].State = state.Completion(instances[i], policy)
		}

		counts[state.Of(instances, !row.DeletedAt.IsZero(), !row.SuspendedAt.IsZero())]++
	}

	for _, workloadState := range state.Workloads {
		r.instruments.workloads.Record(ctx, int64(counts[workloadState]),
			metric.WithAttributes(attribute.String("state", string(workloadState))))
	}
}

// convergeAll converges every workload, several at a time.
//
// Workloads are independent of one another, and converging them in turn made the
// slowest of them the rate at which any of them could be handled. Most of what
// converging costs is waiting — on a daemon, on a process to stop, on an image to
// pull — so one workload waiting used to hold up every workload behind it. Measured
// tearing down a hundred workloads: each of sixty exec workloads waited out its own
// ten-second grace period in turn, and the teardown took five minutes rather than the
// ten seconds it should.
//
// Concurrency is bounded rather than unbounded. A pass over a thousand workloads
// should not open a thousand connections to a daemon that will queue them anyway, and
// a bound keeps the load takt offers a runtime a property of the server rather than of
// how many workloads happen to exist.
func (r *Reconciler) convergeAll(ctx context.Context, rows []database.Workload, observed map[string][]driver.Instance, indexes map[string]map[int]struct{}) {
	var wg sync.WaitGroup

	slots := make(chan struct{}, convergeLimit())

	for _, row := range rows {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			// Shutting down. Whatever has not been reached converges on the next
			// server's first pass, which is what level-triggered reconciliation means.
			wg.Wait()

			return
		}

		wg.Go(func() {
			defer func() { <-slots }()

			ctx, span := r.tracer.Start(ctx, "converge",
				trace.WithAttributes(attribute.String("takt.workload", row.Name)))
			defer span.End()

			started := time.Now()

			err := r.converge(ctx, row, observed[row.Name], indexes[row.Name])

			// Recorded without naming the workload. A histogram carries a series per
			// bucket per attribute, so a name here is sixteen series per workload
			// that outlive the workload itself: an SDK has no way to retire a series,
			// so a node that has run a thousand workloads exports sixteen thousand
			// describing things that no longer exist. A load test left two and a half
			// thousand behind after deleting everything.
			//
			// Per-workload timing is not lost. The converge span above carries the
			// name, and a trace is the signal built for an attribute with one value
			// per thing rather than one per kind of thing.
			r.instruments.converges.Record(ctx, time.Since(started).Seconds())
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				r.logger.With("workload", row.Name, "error", err).Error("failed to reconcile workload")
				r.fail(row.Name, err)
			}
		})
	}

	wg.Wait()
}

// convergeLimit reports how many workloads a pass converges at once.
//
// Scaled to the machine rather than fixed, since the work is mostly waiting and the
// right number is about how much a runtime will accept at once rather than about how
// much computing there is to do. The floor matters on a single-core machine, where a
// limit of one would restore the serial behaviour this exists to avoid.
func convergeLimit() int {
	return max(runtime.NumCPU(), 4)
}

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

		if err := r.stop(ctx, row); err != nil {
			return fmt.Errorf("failed to stop workload for restart: %w", err)
		}

		return r.startAll(ctx, row, count)
	}

	// A scheduled workload runs when its expression says to and waits in between, so
	// the schedule decides rather than the restart policy. Validation refuses a
	// schedule with a count above one, so the whole path converges a single
	// instance.
	if schedule := r.schedule(row); schedule != nil {
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
	}

	byIndex := make(map[int][]driver.Instance, count)
	for _, instance := range instances {
		byIndex[instance.Index] = append(byIndex[instance.Index], instance)
	}

	// At most one replacement of something running per pass, so a change rolls
	// across the instances at the reconcile interval instead of taking them all
	// down at once. Slots with nothing running are not held back by it.
	var (
		replaced   bool
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

	// A specification change is what makes an instance stale, and replacing it is
	// the only way to apply the change. The expected hash is the slot's own: an
	// instance carries the addresses it resolved, and two slots may legitimately
	// carry different ones.
	if stale := r.staleSlot(ctx, row, index, instances); len(stale) > 0 {
		if *replaced {
			// Another slot was replaced this pass. This one is due and rolls on a
			// later pass, which is what keeps a change from taking every instance
			// down at once.
			return slices.ContainsFunc(instances, running), nil
		}

		*replaced = true

		r.logger.With("workload", row.Name, "instance", index, "version", row.Version).Debug("replacing stale instance")

		if err := r.stopInstance(ctx, row, index); err != nil {
			return false, fmt.Errorf("failed to stop stale instance: %w", err)
		}

		return false, r.start(ctx, row, index)
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

	return false, r.restart(ctx, row, index, instances)
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
		if err := r.start(ctx, row, index); err != nil {
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
func (r *Reconciler) staleSlot(ctx context.Context, row database.Workload, index int, instances []driver.Instance) []driver.Instance {
	if len(instances) == 0 {
		return nil
	}

	expected, err := r.slotHash(ctx, row, index)
	if err != nil {
		r.logger.With("workload", row.Name, "instance", index, "error", err).
			Debug("leaving an instance whose expected hash cannot be resolved")

		return nil
	}

	var stale []driver.Instance

	for _, instance := range instances {
		if instance.SpecHash != expected {
			stale = append(stale, instance)
		}
	}

	// The hash says nothing about the slot's own host ports, which live in rows
	// rather than in the specification for every slot but the first. An instance
	// publishing ports its rows no longer name is bound to an address nothing
	// records, which is the same staleness by another route.
	if len(stale) == 0 && portsDrifted(instances, r.slotPorts(row, index)) {
		stale = slices.Clone(instances)
	}

	return stale
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

// slotPorts returns the port rows one instance of a workload holds.
func (r *Reconciler) slotPorts(row database.Workload, index int) []database.Port {
	return slices.DeleteFunc(slices.Clone(r.allocations[row.ID]), func(port database.Port) bool {
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
			case ok && row.DeletedAt.IsZero() && row.SuspendedAt.IsZero() && !retired(restartPolicy(row), byIndex[index]):
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

// schedule reads when a stored workload should run, reporting nil when it runs
// continuously.
//
// A specification that cannot be decoded, or an expression that cannot be parsed, is
// treated as no schedule at all. Both were validated before they were stored, so
// either means the specification and the rules have diverged — and running a workload
// continuously is a better failure than never running it again.
func (r *Reconciler) schedule(row database.Workload) cron.Schedule {
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

// retired reports whether every ended instance is one the policy leaves alone, and so
// whether the workload is finished with rather than waiting to be restarted.
//
// This asks only whether to act. How the workload ended is a separate question the
// caller answers from the exit code, because a workload that will not be restarted
// still has to say whether it succeeded.
//
// A workload with nothing observed at all is not retired. It has yet to run, and
// treating an empty driver as a finished job would mean a workload never started.
func retired(restart *manifest.Restart, instances []driver.Instance) bool {
	if len(instances) == 0 {
		return false
	}

	for _, instance := range instances {
		if restart.Policy.Restarts(instance.ExitCode) {
			return false
		}
	}

	return true
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
func (r *Reconciler) checked(instance driver.Instance) driver.Instance {
	if r.checker == nil || instance.State != driver.StateRunning {
		return instance
	}

	result, ok := r.checker.Result(instance.Workload, instance.Index)
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

			return nil
		}

		r.logger.With("workload", row.Name).Debug("replacing a run still going at its next occurrence")
	}

	// Whatever is there is cleared first: container names derive from the workload and
	// version, so a new run would collide with the one it replaces.
	if err := r.stop(ctx, row); err != nil {
		return fmt.Errorf("failed to clear the previous run: %w", err)
	}

	r.settleAll(row.Name)

	return r.start(ctx, row, 0)
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

	if !slices.ContainsFunc(instances, failed) {
		return nil
	}

	policy := restartPolicy(row)
	if retired(policy, instances) {
		return nil
	}

	return r.restart(ctx, row, 0, instances)
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

	if err := r.workloads.Delete(ctx, row.Name); err != nil {
		return fmt.Errorf("failed to delete workload: %w", err)
	}

	// Backoff and the last error would otherwise outlive the workload, pacing the
	// restarts of a later workload that happens to reuse the name and reporting a
	// failure it never had.
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

	// Backoff and the last error describe attempts to run the workload, which is
	// exactly what suspension asks to stop. Clearing them means a resume starts
	// from a clean slate rather than inside a backoff window.
	r.settleAll(row.Name)

	// A restart asked for before the suspension landed is superseded by it. What
	// was running is stopped either way, and a resume should not replay it.
	r.restartRequested(row.Name)

	return nil
}

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

	if err := r.start(ctx, row, index); err != nil {
		r.hold(ctx, row.Name, index, restartPolicy(row))

		return err
	}

	r.settle(row.Name, index)

	return nil
}

// restart brings back an instance whose runs have all stopped, pacing repeated
// failures with exponential backoff.
func (r *Reconciler) restart(ctx context.Context, row database.Workload, index int, instances []driver.Instance) error {
	if r.waiting(row.Name, index) {
		return nil
	}

	policy := restartPolicy(row)

	// An instance told to give up gives up. It is left exactly as it ended, so the
	// outcome stays readable, and changing the specification starts it again.
	if !policy.Restarts(exitCodeOf(instances), r.attempts(row.Name, index)) {
		if r.giveUp(row.Name, index) {
			r.instruments.giveups.Add(ctx, 1,
				metric.WithAttributes(attribute.String("workload", row.Name)))
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

	if err := r.start(ctx, row, index); err != nil {
		r.hold(ctx, row.Name, index, policy)

		return err
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
	state.next = r.now().Add(delay(state.attempts, restart.Delay))
	r.backoff[key] = state

	r.instruments.restarts.Add(ctx, 1,
		metric.WithAttributes(attribute.String("workload", workload)))

	return state
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

// settle forgets an instance's backoff and its workload's last error, which is what
// starting from a clean slate means: the next failure is paced from the beginning
// rather than from where the last run of failures left off, and nothing is reported
// as wrong until something is.
//
// The error clears here rather than when an attempt begins, so that it stands for
// exactly as long as the workload has not converged. An error that vanished the
// moment a retry began would be invisible for the window an operator is looking.
func (r *Reconciler) settle(workload string, index int) {
	r.mux.Lock()
	defer r.mux.Unlock()

	delete(r.backoff, slot{workload: workload, instance: index})
	delete(r.lastError, workload)
}

// settleAll forgets every instance's backoff and the workload's last error, for the
// paths that act on the whole workload: a teardown, a suspension, a schedule.
func (r *Reconciler) settleAll(workload string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	for key := range r.backoff {
		if key.workload == workload {
			delete(r.backoff, key)
		}
	}

	delete(r.lastError, workload)
}

// fail records why a converge pass over a workload failed, so that the workload can
// report it until a pass succeeds.
func (r *Reconciler) fail(workload string, err error) {
	r.mux.Lock()
	defer r.mux.Unlock()

	r.lastError[workload] = lastError{message: err.Error(), at: r.now()}
}

// LastError reports why the last converge pass over a workload failed and when,
// reporting false when the workload's last pass succeeded or none has run.
func (r *Reconciler) LastError(workload string) (string, time.Time, bool) {
	r.mux.Lock()
	defer r.mux.Unlock()

	recorded, ok := r.lastError[workload]

	return recorded.message, recorded.at, ok
}

func (r *Reconciler) start(ctx context.Context, row database.Workload, index int) error {
	w, err := driver.NewWorkload(row)
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
		if w.Env, err = r.env.Resolve(startCtx, w.Env, w.Name, w.Instance); err != nil {
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
		// it. Deliberately not recorded as the workload's last error, because a
		// pull in progress is nothing an operator has to act on.
		if errors.Is(err, driver.ErrImagePulling) {
			r.logger.With("workload", row.Name, "instance", index).Debug("waiting for image pull")

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

	return nil
}

// driverFor returns the driver that runs a workload's runtime, reporting false when
// nothing does.
func (r *Reconciler) driverFor(row database.Workload) (Driver, bool) {
	d, ok := r.drivers[row.Runtime]

	return d, ok
}

// observe collects the instances every driver is running.
//
// A pass has to see everything, because a workload it cannot see reads as absent and
// would be started again. One driver failing therefore fails the whole observation
// rather than yielding a partial picture that would be acted on as though complete.
//
// How each driver answered is recorded per driver, inside the loop, so a failure
// that abandons the observation still leaves an earlier driver's success on
// record — readiness reports each driver on its own answer, not on the pass.
func (r *Reconciler) observe(ctx context.Context) ([]driver.Instance, error) {
	var instances []driver.Instance

	for name, d := range r.drivers {
		observed, err := r.observeDriver(ctx, name, d)
		if err != nil {
			return nil, fmt.Errorf("failed to observe the %s runtime: %w", name, err)
		}

		instances = append(instances, observed...)
	}

	return instances, nil
}

// observeDriver asks one driver what it is running, recording how it answered and
// how long the answer took.
func (r *Reconciler) observeDriver(ctx context.Context, name string, d Driver) ([]driver.Instance, error) {
	started := time.Now()

	// The span for the call comes from the driver itself, which the server hands
	// over wrapped in telemetry.WrapDriver.
	instances, err := d.Observe(ctx)
	r.recordObservation(name, err)

	r.instruments.observes.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("driver", name),
		telemetry.OutcomeOf(err).Attribute(),
	))

	return instances, err
}

// recordObservation remembers how observing one driver ended, which Observations
// reports.
func (r *Reconciler) recordObservation(name string, err error) {
	observation := Observation{At: r.now()}
	if err != nil {
		observation.Error = err.Error()
	}

	r.obsMux.Lock()
	defer r.obsMux.Unlock()

	r.observations[name] = observation
}

// watch merges every driver's events into one channel, so the loop selects on a single
// source however many runtimes there are.
//
// The merged channel closes once every driver's has. The loop treats that as the end of
// events and falls back to the ticker, which is the same thing it already did when a
// single driver's stream ended.
func (r *Reconciler) watch(ctx context.Context) (<-chan driver.Event, error) {
	streams := make([]<-chan driver.Event, 0, len(r.drivers))
	for _, d := range r.drivers {
		events, err := d.Watch(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to watch the %s runtime: %w", d.Name(), err)
		}

		streams = append(streams, events)
	}

	merged := make(chan driver.Event)

	var wg sync.WaitGroup
	for _, stream := range streams {
		wg.Go(func() {
			for event := range stream {
				select {
				case merged <- event:
				case <-ctx.Done():
					return
				}
			}
		})
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged, nil
}

// abandonPorts gives up the host ports takt chose for a workload, so that the next
// pass tries different ones. Failures are logged rather than returned: the caller is
// already reporting why the workload didn't start, and a workload that keeps its
// ports is no worse off than before.
func (r *Reconciler) abandonPorts(ctx context.Context, row database.Workload, index int) {
	if r.reallocate == nil {
		return
	}

	changed, err := r.reallocate(ctx, row.Name, index)
	switch {
	case err != nil:
		r.logger.With("workload", row.Name, "instance", index, "error", err).Error("failed to reallocate workload ports")
	case changed:
		r.logger.With("workload", row.Name, "instance", index).Info("reallocated host ports after a failed start")
	}
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

	r.mux.Lock()
	delete(r.backoff, slot{workload: row.Name, instance: index})
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
func settled(instance driver.Instance, now time.Time) bool {
	if instance.State != driver.StateRunning {
		return false
	}

	return instance.StartedAt.IsZero() || now.Sub(instance.StartedAt) >= settlePeriod
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

// delay returns how long to wait before the given restart attempt, doubling with
// each consecutive failure up to maxBackoff.
func delay(attempts int, base time.Duration) time.Duration {
	if base <= 0 {
		base = baseBackoff
	}

	d := base << min(attempts, 8)
	if d > maxBackoff || d <= 0 {
		// The shift overflows for a base a workload could legitimately name, so the
		// ceiling catches that as well as an ordinary long wait.
		return maxBackoff
	}

	return d
}
