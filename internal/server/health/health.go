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
		// Signals that the set of checks has changed, so that the loop recomputes
		// when it is next needed rather than sleeping on a stale answer.
		wake chan struct{}
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
		// Incremented every time the specification changes, so that a probe still
		// running against the previous one is recognised as stale when it returns.
		generation int
	}
)

// New returns a Checker ready to run checks.
func New() *Checker {
	return &Checker{
		checks: make(map[string]*check),
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

	// A probe against the specification being replaced may still be running. Counting
	// generations is what lets its result be discarded when it returns: it describes
	// an address or a path this check no longer has.
	var generation int
	if existing, ok := c.checks[workload]; ok {
		generation = existing.generation + 1
	}

	c.checks[workload] = &check{
		spec:       spec,
		started:    time.Now(),
		result:     Result{Status: StatusStarting},
		generation: generation,
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
	for workload, due := range c.due() {
		probes.Go(func() {
			c.record(workload, due.generation, c.probe(ctx, due.spec))
		})
	}
}

// due returns a snapshot of the checks that are ready to run, marking each as in
// flight so that a second probe is not started for a workload still answering the
// first. The snapshot means probing doesn't hold the lock, so a check in flight can't
// block a read of results.
func (c *Checker) due() map[string]check {
	c.mux.Lock()
	defer c.mux.Unlock()

	now := time.Now()
	due := make(map[string]check, len(c.checks))

	for workload, registered := range c.checks {
		if registered.inflight {
			continue
		}

		if registered.result.CheckedAt.IsZero() || now.Sub(registered.result.CheckedAt) >= registered.spec.Interval {
			registered.inflight = true
			due[workload] = *registered
		}
	}

	return due
}

// record stores the outcome of a check, deciding what it means for the workload.
//
// The generation is the one the probe was started against, so that a result arriving
// after the specification changed is discarded rather than attributed to a check it
// says nothing about.
func (c *Checker) record(workload string, generation int, err error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	registered, ok := c.checks[workload]
	if !ok {
		// Forgotten while the check was in flight, so the result describes a workload
		// nothing is running any more. There is no flag left to clear: whatever this
		// probe was started against is gone.
		return
	}

	if registered.generation != generation {
		// Replaced while this probe was running. The check that replaced it was never
		// marked in flight, so there is nothing to clear here either.
		return
	}

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
