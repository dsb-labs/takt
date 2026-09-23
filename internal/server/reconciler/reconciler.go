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

		// The port allocations the current pass converges against, written once as
		// a pass begins and read by the converges it spawns. Unguarded because
		// passes never overlap: the loop's goroutine writes it before anything
		// concurrent reads it.
		allocations map[string][]database.Port

		// Guards backoff and restarts, which are the only state a pass
		// carries between workloads and so the only things converging them
		// concurrently can contend on.
		mux sync.Mutex
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
		observations: observations,
		subscribers:  make(map[chan struct{}]struct{}),
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
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	events, err := r.watch(ctx)
	if err != nil {
		return err
	}

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

// Passes reports how many reconciliation passes have completed.
//
// A pass observes the runtimes before it acts, so nothing a pass does first says that
// it has finished — and workloads are converged concurrently, so the last of them may
// still be in flight when the loop moves on. This counts what a caller actually wants
// to know.
func (r *Reconciler) Passes() uint64 {
	return r.passes.Load()
}

// Subscribe returns a channel that receives a value each time the loop has
// looked at the fleet, until ctx ends. It is the push form of Passes, for a
// caller that wants to act on what a pass found rather than count them.
//
// A pass signals twice: once it has observed the drivers, and again once it has
// finished acting on what it saw. The first is what makes a subscriber prompt.
// A pass acting on a new workload pulls its image, and a subscriber waiting for
// the end of that would learn of a change that had nothing to do with the pull
// only once the pull was done. The second is what reports the pass's own work,
// since starting and stopping instances is what most changes the fleet.
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
		r.looked()
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

// driverFor returns the driver that runs a workload's runtime, reporting false when
// nothing does.
func (r *Reconciler) driverFor(row database.Workload) (Driver, bool) {
	d, ok := r.drivers[row.Runtime]

	return d, ok
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
