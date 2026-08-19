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
	}

	// The check type is one workload's check and the state of running it.
	check struct {
		spec    Check
		started time.Time
		result  Result
	}
)

// New returns a Checker ready to run checks.
func New() *Checker {
	return &Checker{
		checks: make(map[string]*check),
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

	c.checks[workload] = &check{
		spec:    spec,
		started: time.Now(),
		result:  Result{Status: StatusStarting},
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
func (c *Checker) Run(ctx context.Context) error {
	timer := time.NewTimer(c.wait())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			c.checkDue(ctx)
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
		if registered.result.CheckedAt.IsZero() {
			return 0
		}

		if due := registered.spec.Interval - now.Sub(registered.result.CheckedAt); due < wait {
			wait = due
		}
	}

	return max(wait, 0)
}

// checkDue probes every workload whose interval has elapsed.
func (c *Checker) checkDue(ctx context.Context) {
	var wg sync.WaitGroup

	for workload, due := range c.due() {
		wg.Go(func() {
			c.record(workload, c.probe(ctx, due.spec))
		})
	}

	// Waiting means a slow check delays the next tick rather than overlapping with
	// itself, so a workload cannot accumulate probes faster than they complete.
	wg.Wait()
}

// due returns a snapshot of the checks that are ready to run. The snapshot means
// probing doesn't hold the lock, so a check in flight can't block a read of results.
func (c *Checker) due() map[string]check {
	c.mux.RLock()
	defer c.mux.RUnlock()

	now := time.Now()
	due := make(map[string]check, len(c.checks))

	for workload, registered := range c.checks {
		if registered.result.CheckedAt.IsZero() || now.Sub(registered.result.CheckedAt) >= registered.spec.Interval {
			due[workload] = *registered
		}
	}

	return due
}

// record stores the outcome of a check, deciding what it means for the workload.
func (c *Checker) record(workload string, err error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	registered, ok := c.checks[workload]
	if !ok {
		// Forgotten while the check was in flight, so the result describes a workload
		// nothing is running any more.
		return
	}

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
