// Package reconciler provides the control loop that drives the running state of
// the node towards the desired state held in the database.
package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/event"
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
		// names, reporting an error when one cannot be resolved. The reader's
		// identifier is what a token minted for the instance is bound to.
		Resolve(ctx context.Context, env map[string]string, readerID, reader string, readerInstance int) (map[string]string, error)
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

	// The Tokens interface describes how the reconciler revokes the tokens minted
	// for a workload as what they were minted for goes away.
	//
	// Only revocation lives here. Minting happens where the credential is used —
	// the resolver for an environment, the mounter for a file — but what ends a
	// credential's life is the instance lifecycle, which is the reconciler's.
	Tokens interface {
		// RevokeForWorkload should revoke every token minted for the workload.
		RevokeForWorkload(ctx context.Context, workloadID string) error
		// RevokeForInstance should revoke every token minted for one instance
		// of the workload, leaving the tokens its instances share alone.
		RevokeForInstance(ctx context.Context, workloadID string, instance int) error
		// RevokeSuperseded should revoke the shared tokens of every workload
		// version but the given one.
		RevokeSuperseded(ctx context.Context, workloadID string, version int) error
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
		// Changed should return a channel that receives a value when an
		// instance's verdict changes.
		Changed() <-chan struct{}
	}

	// The Recorder interface describes how the reconciler records what it observed
	// about a workload while converging it.
	//
	// A pass converges workloads concurrently, so an implementation must be safe to
	// call from several goroutines at once.
	Recorder interface {
		// Record should record an event against the named workload, coalescing it
		// with an event already recorded carrying the same reason and data.
		Record(ctx context.Context, workload string, reason event.Reason, data []byte) error
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
		tokens      Tokens
		checker     Checker
		events      Recorder
		bind        string
		reallocate  func(ctx context.Context, workload string, instance int) (bool, error)
		now         func() time.Time
		interval    time.Duration
		hostPaths   []string
		nudge       chan struct{}
		tracer      trace.Tracer
		instruments instruments

		// The port allocations the most recent pass read, replaced wholesale as
		// each pass begins. A converge outlives the pass that spawned it, so one
		// may read the allocations a later pass wrote. That is the newer truth
		// about the same rows, and never a map mid-write.
		allocations atomic.Pointer[map[string][]database.Port]

		// Bounds how many converges act at once, across passes rather than
		// within one. The runtime behind them is a single daemon, and a pass
		// that spawned an unbounded number of stops against it would be trading
		// a slow pass for a saturated daemon.
		slots chan struct{}
		// Counts the goroutines a pass spawned and the loop has not waited for:
		// the converges, the orphan stops, and the completion that counts the
		// pass once they finish. Run waits on it before returning, so none
		// outlives the reconciler.
		work sync.WaitGroup
		// Closed once the most recent pass has been counted as complete. Only
		// the loop's goroutine touches it, on its way to spawning the next
		// completion, which waits on it so passes are counted in order.
		completed chan struct{}

		// Guards backoff, restarts and converging, which are the only state a
		// pass carries between workloads and so the only things converging them
		// concurrently can contend on.
		mux sync.Mutex
		// The workloads a converge spawned by an earlier pass is still acting
		// on. A pass leaves such a workload alone: the state it observed is
		// mid-change, and two converges acting on one workload would race each
		// other for its slots.
		converging map[string]struct{}
		// How long to wait before restarting each instance that keeps failing.
		// Keyed per instance, so one instance crashing does not pace the others.
		backoff map[slot]backoff
		// The instance whose ending has already been recorded against each slot,
		// keyed by the instance's own identifier.
		//
		// A pass is level-triggered, so an instance that ended is seen ended by
		// every pass until something replaces it. Without this, one ending would be
		// recorded as an event per pass for as long as the corpse stayed, and its
		// count would report how long nobody looked rather than how often the
		// workload ended.
		exits map[slot]string
		// The health verdict already recorded against each slot, so that a check
		// holding steady is not recorded on every pass that reads it.
		verdicts map[slot]health.Status
		// The instance the checker most recently failed in each slot, keyed by the
		// instance's own identifier.
		//
		// A failed check rewrites a running instance to a failed one, and from then
		// on the pass cannot tell it from a process that stopped. This is what lets
		// the ending be recorded as the check's doing rather than as an exit the
		// process never made.
		unhealthy map[slot]string
		// The workloads whose instances an operator asked to have replaced,
		// consumed by the next pass over each. In memory rather than stored,
		// because a request the server loses can simply be made again.
		restarts map[string]struct{}
		// Counts completed passes, so that a caller can tell a pass has finished
		// rather than inferring it from something a pass happens to do first.
		passes atomic.Uint64
		// Counts the passes the loop has run, whether or not their converges
		// have finished, for the work a pass does every so often. Only the
		// loop's goroutine touches it.
		began uint64

		// Guards observations, separately from mux so a caller polling readiness
		// never contends with a pass's backoff bookkeeping.
		obsMux sync.Mutex
		// How the most recent observation of each driver ended, keyed by runtime
		// name. Seeded with a zero value per driver so a driver that has never
		// answered reads as not ready rather than as absent.
		observations map[string]Observation

		// Guards subscribers, separately from mux for the reason obsMux is.
		subMux sync.Mutex
		// The channels told when a pass completes, each buffered by one so that
		// a subscriber which has not caught up holds one pending signal rather
		// than a queue of them.
		subscribers map[chan struct{}]struct{}
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
		// Revokes the tokens minted for a workload as its instances go. May be
		// nil, in which case nothing is revoked — so one is only ever nil where
		// no workload holds a token, as in tests.
		Tokens Tokens
		// Reports what takt's own health checks established. May be nil, in which
		// case only the state the driver reports is acted on.
		Checker Checker
		// Records what a pass observed about a workload, which is what an operator
		// reads to learn why it looks the way it does. May be nil, in which case
		// nothing is recorded.
		Events Recorder
		// The address a workload's host ports are published on, which is where a
		// health check is performed. Empty probes loopback.
		Bind string
		// Called to abandon the host ports takt chose for one instance of a
		// workload when it fails to start, reporting whether anything changed. May
		// be nil, in which case ports are never reallocated.
		Reallocate func(ctx context.Context, workload string, instance int) (bool, error)
		// How often a full reconciliation pass runs regardless of events.
		Interval time.Duration
		// The prefixes a path mount may sit beneath. Each path mount is resolved
		// against them as an instance starts, so a link swapped in after the apply
		// cannot carry the mount elsewhere.
		AllowHostPaths []string
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

	// The slot type identifies one instance of one workload, which is the grain
	// backoff and health are kept at.
	slot struct {
		workload string
		instance int
	}
)

const (
	// How long a converge will wait on the driver before giving up on it.
	//
	// A converge holds its workload for as long as the driver takes to answer, and
	// holds one of the slots every converge shares, so a driver that never answers
	// doesn't just delay one workload — it holds a slot the rest queue behind, and
	// a docker daemon that has wedged will do exactly that. The deadline is
	// generous enough for a slow daemon under load and short enough that a stuck
	// one costs a converge rather than the node.
	//
	// Starting a workload gets its own, longer deadline: it may have to pull an image
	// first, which is legitimately slow and not a sign that anything is wrong.
	driverTimeout = 30 * time.Second
	// How long starting a workload may take, including pulling its image.
	startTimeout = 10 * time.Minute
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
		tokens:       config.Tokens,
		checker:      config.Checker,
		events:       config.Events,
		bind:         config.Bind,
		reallocate:   config.Reallocate,
		now:          clock(config.Now),
		interval:     config.Interval,
		hostPaths:    config.AllowHostPaths,
		backoff:      make(map[slot]backoff),
		exits:        make(map[slot]string),
		verdicts:     make(map[slot]health.Status),
		unhealthy:    make(map[slot]string),
		restarts:     make(map[string]struct{}),
		converging:   make(map[string]struct{}),
		observations: observations,
		subscribers:  make(map[chan struct{}]struct{}),
		tracer:       telemetry.Tracer(config.TracerProvider, scope),
		instruments:  newInstruments(telemetry.Meter(config.MeterProvider, scope)),
		slots:        make(chan struct{}, convergeLimit()),
		// Closed from the start, so the first pass has no earlier pass to wait
		// on before it is counted.
		completed: closed(),
		// Buffered so that a caller signalling a change never blocks: a pass is
		// already pending, which is all the signal conveys.
		nudge: make(chan struct{}, 1),
	}
}

// closed returns a channel that is already closed.
func closed() chan struct{} {
	done := make(chan struct{})
	close(done)

	return done
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

// record stores an event against a workload, saying what this pass observed
// about it.
//
// A failure here is logged rather than returned. Recording is an observation of
// a converge rather than a step in one, so a pass that cannot write down what it
// saw has still done its job, and every caller is somewhere that would otherwise
// abandon real work to report it.
//
// Callers must not hold r.mux. The write goes to the database, so holding the
// lock across it would serialise a concurrent pass behind a disk write.
func (r *Reconciler) record(ctx context.Context, workload string, reason event.Reason, fields event.Fields) {
	if r.events == nil {
		return
	}

	if err := r.events.Record(ctx, workload, reason, event.Encode(fields)); err != nil {
		r.logger.With("error", err, "workload", workload, "reason", reason).Error("failed to record workload event")
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
// Passes run on a ticker, when the driver reports a change, when a health check's
// verdict changes, and when Notify is called. Passes never overlap: each is
// driven from this one goroutine. A driver event does not run a pass alone — the
// events behind it are collected for a short window first, so a burst coalesces
// into one pass rather than queueing one each. A verdict is held for the same
// window, since the instances of one workload tend to come up together.
//
// A pass decides and the converges it spawns act, and the pass does not wait for
// them. Stopping an instance takes as long as its grace period, and a pass that
// waited on one would hold every other workload's restart, replacement and
// deletion behind it. The converges run on until ctx is cancelled, and this
// waits for them before returning so none outlives the reconciler.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	events, err := r.watch(ctx)
	if err != nil {
		return err
	}

	defer r.work.Wait()

	// Nil without a checker, which a select never receives from.
	var verdicts <-chan struct{}
	if r.checker != nil {
		verdicts = r.checker.Changed()
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
		case <-verdicts:
			r.logger.Debug("reconciling after a health verdict changed")
			coalesce(ctx, events)
			r.reconcile(ctx)
		case event, ok := <-events:
			if !ok {
				// The merged stream closes only once ctx has ended and every
				// forwarder has returned, so there is nothing left to select on.
				events = nil

				continue
			}

			r.logger.With("workload", event.Workload).Debug("reconciling after driver event")
			coalesce(ctx, events)
			r.reconcile(ctx)
		}
	}
}

// Passes reports how many reconciliation passes have completed, converges
// included.
//
// A pass observes the runtimes before it acts, so nothing a pass does first says that
// it has finished — and the converges it spawns outlive it, so the loop moving on
// says nothing either. A pass is counted once every converge it spawned has
// finished, and after the pass before it was counted, so the count says how many
// passes have done all they set out to do.
func (r *Reconciler) Passes() uint64 {
	return r.passes.Load()
}

// Subscribe returns a channel that receives a value each time the loop has
// looked at the fleet, until ctx ends. It is the push form of Passes, for a
// caller that wants to act on what a pass found rather than count them.
//
// A pass signals twice: once it has observed the drivers, and again once the
// converges it spawned have finished acting on what it saw. The first is what
// makes a subscriber prompt. A pass acting on a new workload pulls its image, and
// a subscriber waiting for the end of that would learn of a change that had
// nothing to do with the pull only once the pull was done. The second is what
// reports the pass's own work, since starting and stopping instances is what most
// changes the fleet.
//
// The signal says "look again" and nothing more. What such a caller wants is the
// hydrated view of the fleet — health folded in, ports resolved, retained
// instances removed — which the workload service already builds from a fresh
// observation, so a pass hands out no view of its own. That keeps the signal
// correct however a pass ended: one that failed to observe still completes, and
// the subscriber's own read reports whatever the drivers say now.
//
// A subscriber that has not drained its channel holds one pending signal rather
// than a queue of them. It reads the whole fleet when it looks, so the signals
// it missed told it nothing the next read will not.
func (r *Reconciler) Subscribe(ctx context.Context) <-chan struct{} {
	subscriber := make(chan struct{}, 1)

	r.subMux.Lock()
	r.subscribers[subscriber] = struct{}{}
	r.subMux.Unlock()

	context.AfterFunc(ctx, func() {
		r.subMux.Lock()
		defer r.subMux.Unlock()

		delete(r.subscribers, subscriber)
	})

	return subscriber
}

// looked tells every subscriber that the loop has looked at the fleet. It never
// blocks: a subscriber already holding a signal has all this one would tell it.
func (r *Reconciler) looked() {
	r.subMux.Lock()
	defer r.subMux.Unlock()

	for subscriber := range r.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

// reconcile runs a single pass. Errors affecting one workload are logged and the
// pass continues, so one broken workload can't stop the others converging.
func (r *Reconciler) reconcile(ctx context.Context) {
	started := time.Now()
	outcome := telemetry.OutcomeOK

	ctx, span := r.tracer.Start(ctx, "reconcile")

	// The converges this pass spawns, which the pass does not wait for and its
	// completion does.
	var spawned sync.WaitGroup

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

		r.began++
		r.complete(&spawned)
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

	// Before acting on the observation, so a subscriber is not kept waiting on
	// whatever acting on it takes.
	r.looked()

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

		observed[instance.Workload] = append(observed[instance.Workload], r.checked(ctx, instance))
	}

	// Read once per pass rather than per workload, and published before the
	// converges spawn, so they read it without contention.
	allocations := make(map[string][]database.Port)
	if r.ports != nil {
		if allocations, err = r.ports.ListAll(ctx); err != nil {
			outcome = outcomeObserveFailed
			r.logger.With("error", err).Error("failed to read workload ports")

			return
		}
	}

	r.allocations.Store(&allocations)

	r.measure(ctx, rows, observed)
	r.register(rows, observed)

	desired := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		desired[row.Name] = struct{}{}
	}

	skipped := r.convergeAll(ctx, &spawned, rows, observed, indexes)

	// A pass cut short by shutdown stops here. The sweep and the prune would run
	// against a cancelled context, and every failure they reported would be the
	// cancellation rather than anything about the workloads.
	if ctx.Err() != nil {
		return
	}

	// Anything a driver holds that nothing asked for is an orphan — most often the
	// remnant of a workload deleted while the server was down.
	for workload := range held {
		if _, ok := desired[workload]; ok {
			continue
		}

		if !r.begin(workload) {
			skipped++

			continue
		}

		r.logger.With("workload", workload).Debug("stopping orphaned workload")

		r.spawn(&spawned, func() {
			defer r.finish(workload)

			if err := r.stopOrphan(ctx, workload); err != nil {
				r.logger.With("workload", workload, "error", err).Error("failed to stop orphaned workload")
			}
		})
	}

	span.SetAttributes(attribute.Int("takt.skipped", skipped))

	r.prune(rows)
}

// complete counts the pass once the converges it spawned have finished, and tells
// the subscribers the pass has finished acting.
//
// Passes are counted in the order they ran. A pass that spawned nothing would
// otherwise be counted ahead of one still stopping an instance, and a caller
// waiting on the count to know that work was done would be told it was while it
// was still in flight.
func (r *Reconciler) complete(spawned *sync.WaitGroup) {
	previous := r.completed
	done := make(chan struct{})
	r.completed = done

	r.work.Go(func() {
		defer close(done)

		spawned.Wait()
		<-previous

		r.passes.Add(1)
		r.looked()
	})
}

// spawn runs fn on a goroutine of its own, counted against the pass that spawned
// it and against the reconciler, so the pass can be counted complete once it
// finishes and Run can wait for it before returning.
func (r *Reconciler) spawn(spawned *sync.WaitGroup, fn func()) {
	spawned.Add(1)

	r.work.Go(func() {
		defer spawned.Done()

		fn()
	})
}

// begin claims a workload for a converge, reporting false when one spawned by an
// earlier pass still holds it.
func (r *Reconciler) begin(workload string) bool {
	r.mux.Lock()
	defer r.mux.Unlock()

	if _, ok := r.converging[workload]; ok {
		return false
	}

	r.converging[workload] = struct{}{}

	return true
}

// finish releases a workload once the converge holding it has finished, so the
// next pass acts on it again.
func (r *Reconciler) finish(workload string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	delete(r.converging, workload)
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

	if r.mounts == nil || r.began%every != 0 {
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
//
// The pass does not wait for the converges. What they wait on — a grace period, a
// daemon under load — is time the loop would otherwise spend not looking, and a
// workload deleted or crashing while one instance sat out its grace period used to
// wait the whole period to be seen. A workload still held by an earlier pass's
// converge is skipped, and the count of those is returned for the pass's span: the
// observation it was made from is mid-change, and a second converge would race the
// first for its slots. The next pass after the converge finishes acts on it.
func (r *Reconciler) convergeAll(
	ctx context.Context,
	spawned *sync.WaitGroup,
	rows []database.Workload,
	observed map[string][]driver.Instance,
	indexes map[string]map[int]struct{},
) int {
	var skipped int

	for _, row := range rows {
		if !r.begin(row.Name) {
			r.logger.With("workload", row.Name).Debug("leaving a workload an earlier pass is still converging")
			skipped++

			continue
		}

		r.spawn(spawned, func() {
			defer r.finish(row.Name)

			// A slot is taken here rather than before spawning, so a pass never
			// waits on a converge for one to come free. Shutting down releases
			// whatever is queued: it converges on the next server's first pass,
			// which is what level-triggered reconciliation means.
			select {
			case r.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-r.slots }()

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

				// The event that replaced lastError, and recorded in the one place
				// every converge failure passes through so no path has to remember
				// to report its own. A failure that persists repeats every pass,
				// which coalescing folds into one row with a climbing count.
				//
				// A start that was paced is the one failure already recorded, by
				// the pacing that named the wait, so recording it here again would
				// say the same thing twice.
				if !errors.Is(err, errPaced) {
					r.record(ctx, row.Name, event.ConvergeFailed, event.Fields{Error: err.Error()})
				}
			}
		})
	}

	return skipped
}

// convergeLimit reports how many workloads are converged at once.
//
// Scaled to the machine rather than fixed, since the work is mostly waiting and the
// right number is about how much a runtime will accept at once rather than about how
// much computing there is to do. The floor matters on a single-core machine, where a
// limit of one would restore the serial behaviour this exists to avoid.
func convergeLimit() int {
	return max(runtime.NumCPU(), 4)
}

// driverFor returns the driver that runs a workload's runtime, reporting false when
// nothing does.
func (r *Reconciler) driverFor(row database.Workload) (Driver, bool) {
	d, ok := r.drivers[row.Runtime]

	return d, ok
}
