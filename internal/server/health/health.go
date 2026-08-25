// Package health provides checking of whether a workload is working, as distinct from
// whether its runtime reports it started.
//
// Checks are performed by the server against the workload's published address rather
// than by the runtime inside the workload. That is what lets an image carrying no
// shell be checked at all — a distroless image has no interpreter to run a command in
// — and it means a driver only has to expose an address to inherit the behaviour
// rather than implement checking of its own.
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/orca/internal/server/telemetry"
)

// The Status type describes whether a workload is working.
type Status string

const (
	// StatusStarting indicates the workload has not yet passed a check, either
	// because it is inside its start period or because nothing has answered yet.
	StatusStarting Status = "starting"
	// StatusHealthy indicates the workload's most recent check passed.
	StatusHealthy Status = "healthy"
	// StatusUnhealthy indicates enough consecutive checks have failed to exhaust the
	// workload's retries.
	StatusUnhealthy Status = "unhealthy"
)

type (
	// The Check type describes what to probe and how often.
	Check struct {
		// The address to probe, in host:port form.
		Address string
		// The path to request when checking over HTTP. Empty means the check is a
		// connection attempt rather than a request.
		HTTP string
		// How often to perform the check.
		Interval time.Duration
		// How long a single check may take before it counts as failed.
		Timeout time.Duration
		// How many consecutive failures mark the workload unhealthy.
		Retries int
		// How long after the check is registered to allow before failures count.
		StartPeriod time.Duration
	}

	// The Result type reports the outcome of checking one workload.
	Result struct {
		// Whether the workload is working.
		Status Status
		// How many consecutive checks have failed, reset by one that passes.
		Failures int
		// When the check last ran.
		CheckedAt time.Time
		// Why the last check failed, when it did.
		Error string
	}

	// The Checker type performs a workload's health checks and remembers their most
	// recent outcome.
	//
	// Results are held in memory rather than persisted because a check describes a
	// moment rather than an intention. A restarted server should establish whether a
	// workload is working now, not trust a verdict reached before it stopped.
	Checker struct {
		mux    sync.RWMutex
		client *http.Client
		checks map[string]*check
		// Counts probes so each has an identity of its own. Guarded by mux rather
		// than atomic, since it is only ever touched while holding it.
		probes uint64
		// Signals that the set of checks has changed, so that the loop recomputes
		// when it is next needed rather than sleeping on a stale answer.
		wake chan struct{}
		// The meters probe results are recorded into.
		instruments instruments
	}

	// The Config type contains fields used to construct a Checker.
	Config struct {
		// The meter the checker's instruments are created from. May be nil, in
		// which case nothing is recorded.
		Meter metric.Meter
	}

	// The scheduled type is one check as handed to a probe: the specification to
	// probe, the context that stops it when the check is replaced or dropped, and the
	// identity that decides whether its result is still wanted.
	scheduled struct {
		spec   Check
		ctx    context.Context
		cancel context.CancelFunc
		probe  uint64
	}

	// The check type is one workload's check and the state of running it.
	check struct {
		spec    Check
		started time.Time
		result  Result
		// Whether a probe for this workload is currently running. A check is not due
		// while one is, which is what keeps a workload from accumulating probes
		// faster than they complete without making it wait on any other workload's.
		inflight bool
		// Cancels the probe currently in flight, so that replacing or dropping a
		// check stops the work being done against the specification it replaced
		// rather than leaving it to run out its timeout. Nil when none is running.
		cancel context.CancelFunc
		// Identifies the probe in flight, so that a result arriving from one started
		// against a specification since replaced is recognised and discarded.
		probe uint64
	}
)

// New returns a Checker ready to run checks.
func New(config Config) *Checker {
	return &Checker{
		instruments: newInstruments(config.Meter),
		checks:      make(map[string]*check),
		// Buffered so that registering a check never blocks on the loop: a
		// recomputation is already pending, which is all the signal conveys.
		wake: make(chan struct{}, 1),
		// Redirects are not followed: a health endpoint answering 302 is telling us
		// something other than "I am working", and following it would check a
		// different address than the one asked for.
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Set registers the check for a workload, replacing any it already had.
//
// A check whose specification or address has changed starts afresh, since failures
// counted against the old one say nothing about the new. An unchanged check keeps its
// history, so re-applying a manifest doesn't reset a workload's start period and let
// a broken workload look like it is starting again.
func (c *Checker) Set(workload string, spec Check) {
	c.mux.Lock()
	defer c.mux.Unlock()

	if existing, ok := c.checks[workload]; ok && existing.spec == spec {
		return
	}

	// A probe against the specification being replaced may still be running. It is
	// checking an address or a path this workload no longer has, so it is stopped
	// rather than left to finish: its verdict is meaningless, and on a workload that
	// never answers it would otherwise hold a connection open for its whole timeout.
	if existing, ok := c.checks[workload]; ok && existing.cancel != nil {
		existing.cancel()
	}

	c.checks[workload] = &check{
		spec:    spec,
		started: time.Now(),
		result:  Result{Status: StatusStarting},
	}

	// A newly registered check is due immediately, and the loop may be sleeping on a
	// wait computed before it existed — for as long as a second when nothing else is
	// waiting, or a whole interval otherwise.
	c.notify()
}

// notify tells the loop that the set of checks has changed. It never blocks.
func (c *Checker) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Forget drops the check for a workload, which the caller does once the workload is
// gone so that results don't accumulate for work nothing runs any more.
func (c *Checker) Forget(workload string) {
	c.mux.Lock()
	defer c.mux.Unlock()

	// Nothing runs this workload any more, so a probe still in flight against it is
	// work being done on behalf of something gone.
	if existing, ok := c.checks[workload]; ok && existing.cancel != nil {
		existing.cancel()
	}

	delete(c.checks, workload)
}

// Result returns the most recent outcome for a workload, reporting false when it has
// no check registered.
func (c *Checker) Result(workload string) (Result, bool) {
	c.mux.RLock()
	defer c.mux.RUnlock()

	registered, ok := c.checks[workload]
	if !ok {
		return Result{}, false
	}

	return registered.result, true
}

// Run performs registered checks until ctx is cancelled, returning nil on a clean
// shutdown.
//
// Every workload is checked from this one loop rather than each on its own timer, so
// the number of goroutines is a property of the server rather than of how many
// workloads happen to be registered.
//
// The loop wakes when the soonest check is next due rather than at a fixed rate. A
// fixed tick would quietly become a floor on how often anything could be checked, so
// a workload asking for a half-second interval would get whatever the tick was.
//
// Probes run on their own goroutines and the loop does not wait for them, so one
// workload that accepts a connection and never answers cannot delay the checking of
// any other. The loop waits only for them to finish before it returns, so a probe
// never outlives the Checker.
//
// Registering a check wakes the loop rather than waiting for whatever it was already
// sleeping on, so a workload is checked as soon as it is known about.
func (c *Checker) Run(ctx context.Context) error {
	timer := time.NewTimer(c.wait())
	defer timer.Stop()

	var probes sync.WaitGroup
	defer probes.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			c.checkDue(ctx, &probes)
			timer.Reset(c.wait())
		case <-c.wake:
			// A check was registered, which may be due sooner than whatever the timer
			// was waiting for. Stopping it before resetting keeps the two in step.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			c.checkDue(ctx, &probes)
			timer.Reset(c.wait())
		}
	}
}

// wait returns how long until the soonest check is due.
//
// With nothing registered there is nothing to wait for, so the loop settles on a
// modest idle period rather than spinning: a workload registered in the meantime is
// checked when that elapses, and its first check is due immediately anyway.
func (c *Checker) wait() time.Duration {
	c.mux.RLock()
	defer c.mux.RUnlock()

	if len(c.checks) == 0 {
		return time.Second
	}

	now := time.Now()
	wait := time.Second

	for _, registered := range c.checks {
		// A probe already running has no next-due time to compute: it is neither
		// waiting nor overdue, and treating an unanswered check as due immediately
		// would spin the loop for as long as the probe took.
		if registered.inflight {
			continue
		}

		if registered.result.CheckedAt.IsZero() {
			return 0
		}

		if due := registered.spec.Interval - now.Sub(registered.result.CheckedAt); due < wait {
			wait = due
		}
	}

	return max(wait, 0)
}

// checkDue starts a probe for every workload whose interval has elapsed, on probes so
// that Run can wait for them at shutdown without waiting for them here.
func (c *Checker) checkDue(ctx context.Context, probes *sync.WaitGroup) {
	for workload, due := range c.due(ctx) {
		probes.Go(func() {
			// Releases the context whether the probe answered, timed out, or was
			// cancelled by its check being replaced.
			defer due.cancel()

			started := time.Now()
			err := c.probe(due.ctx, due.spec)

			c.instruments.duration.Record(due.ctx, time.Since(started).Seconds(), metric.WithAttributes(
				attribute.String("workload", workload),
				telemetry.OutcomeOf(err).Attribute(),
			))

			c.record(workload, due.probe, err)
		})
	}
}

// due returns the checks that are ready to run, marking each as in flight so that a
// second probe is not started for a workload still answering the first, and giving
// each a context so that replacing or dropping the check stops its probe.
//
// The returned specifications are copies, so probing doesn't hold the lock and a check
// in flight can't block a read of results.
func (c *Checker) due(ctx context.Context) map[string]scheduled {
	c.mux.Lock()
	defer c.mux.Unlock()

	now := time.Now()
	due := make(map[string]scheduled, len(c.checks))

	for workload, registered := range c.checks {
		if registered.inflight {
			continue
		}

		if registered.result.CheckedAt.IsZero() || now.Sub(registered.result.CheckedAt) >= registered.spec.Interval {
			probeCtx, cancel := context.WithCancel(ctx)

			c.probes++

			registered.inflight = true
			registered.cancel = cancel
			registered.probe = c.probes

			due[workload] = scheduled{
				spec:   registered.spec,
				ctx:    probeCtx,
				cancel: cancel,
				probe:  c.probes,
			}
		}
	}

	return due
}

// record stores the outcome of a check, deciding what it means for the workload.
//
// The probe identifier is the one stamped on the check when it was started, so a
// result arriving after the check was replaced belongs to a specification that no
// longer exists and is discarded rather than attributed to one it says nothing about.
func (c *Checker) record(workload string, probe uint64, err error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	registered, ok := c.checks[workload]
	if !ok {
		// Forgotten while the check was in flight, so the result describes a workload
		// nothing is running any more.
		return
	}

	if registered.probe != probe {
		// Replaced while this probe was running, and the check that replaced it owns
		// its own probe state — so there is nothing to clear here.
		return
	}

	registered.cancel = nil

	// Cleared however this probe turned out, so that a failed check doesn't leave the
	// workload permanently ineligible for the next one.
	registered.inflight = false

	registered.result.CheckedAt = time.Now()

	if err == nil {
		registered.result.Status = StatusHealthy
		registered.result.Failures = 0
		registered.result.Error = ""

		return
	}

	registered.result.Failures++
	registered.result.Error = err.Error()

	// Inside the start period a failure is expected rather than meaningful: a
	// workload that takes time to become ready would otherwise be replaced for
	// failing checks it was never going to pass yet. Once it has passed one, the
	// start period is over regardless of the clock — it has demonstrably started.
	warming := registered.result.Status == StatusStarting &&
		time.Since(registered.started) < registered.spec.StartPeriod

	if !warming && registered.result.Failures >= registered.spec.Retries {
		registered.result.Status = StatusUnhealthy
	}
}

// probe performs a single check, returning nil when the workload answered.
func (c *Checker) probe(ctx context.Context, spec Check) error {
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	if spec.HTTP == "" {
		return c.probeTCP(ctx, spec.Address)
	}

	return c.probeHTTP(ctx, spec.Address, spec.HTTP)
}

func (c *Checker) probeTCP(ctx context.Context, address string) error {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	return conn.Close()
}

func (c *Checker) probeHTTP(ctx context.Context, address, path string) error {
	url := "http://" + address + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to construct request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request %s: %w", path, err)
	}
	defer resp.Body.Close()

	// Any 2xx counts as working. A health endpoint is answering the question "can you
	// serve", and a body nobody reads is not part of the answer.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s answered %s", path, strconv.Itoa(resp.StatusCode))
	}

	return nil
}
