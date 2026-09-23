package reconciler

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/telemetry"
)

type (
	// The Observation type records how the most recent attempt to observe one
	// driver ended, which is what the readiness endpoint reports.
	Observation struct {
		// When the driver was last asked. Zero when it has not been asked yet.
		At time.Time
		// What the driver answered. Empty when it succeeded.
		Error string
	}
)

const (
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

	// The wait before a driver's event stream is watched again after it ends,
	// doubled on each consecutive failure up to maxRewatchDelay. A daemon
	// restarting is the usual cause, and comes back in seconds.
	baseRewatchDelay = time.Second

	// The ceiling on the wait between attempts to watch a driver again.
	maxRewatchDelay = time.Minute
)

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
// A driver's stream ending is not the end of its events. The daemon behind it may
// have restarted, and a server that stopped listening would sit at the interval's
// latency until it was itself restarted. So each stream is watched again when it
// ends, with a growing wait between attempts, and a pass is asked for once it is
// back to cover whatever happened in between. The merged channel closes only when
// ctx does.
//
// The first watch of each driver is made here rather than in the forwarder, so a
// runtime that cannot be watched at all fails the server at startup.
func (r *Reconciler) watch(ctx context.Context) (<-chan driver.Event, error) {
	type stream struct {
		driver Driver
		events <-chan driver.Event
	}

	streams := make([]stream, 0, len(r.drivers))
	for _, d := range r.drivers {
		events, err := d.Watch(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to watch the %s runtime: %w", d.Name(), err)
		}

		streams = append(streams, stream{driver: d, events: events})
	}

	merged := make(chan driver.Event)

	var wg sync.WaitGroup
	for _, s := range streams {
		wg.Go(func() { r.forward(ctx, s.driver, s.events, merged) })
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	return merged, nil
}

// forward copies one driver's events onto merged until ctx ends, watching the
// driver again each time its stream closes.
func (r *Reconciler) forward(ctx context.Context, d Driver, events <-chan driver.Event, merged chan<- driver.Event) {
	delay := baseRewatchDelay

	for {
		for event := range events {
			select {
			case merged <- event:
			case <-ctx.Done():
				return
			}
		}

		if ctx.Err() != nil {
			return
		}

		r.logger.With("driver", d.Name(), "delay", delay).Warn("driver event stream ended, watching again")

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		var err error
		if events, err = d.Watch(ctx); err != nil {
			r.logger.With("driver", d.Name(), "error", err).Warn("failed to watch driver again")
			events = nil
			delay = min(delay*2, maxRewatchDelay)

			continue
		}

		// Anything the driver reported while nothing was listening is gone, and
		// the next pass observes the whole runtime, so one is asked for now rather
		// than left to the ticker.
		r.logger.With("driver", d.Name()).Info("driver event stream restored")
		r.Notify()

		delay = baseRewatchDelay
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
