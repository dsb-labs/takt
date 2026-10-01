// Package job provides running of the work the server does on its own schedule,
// as distinct from the work it does in answer to a request or a change.
//
// A job is a function that is run once when the runner starts and again every
// interval after that, and whose failure is a warning rather than a reason to stop:
// the next tick tries again. It is the shape of a periodic sweep — expiring
// tokens, asking a registry whether a tag has moved — rather than of a loop with
// state of its own. The reconciler and the health checker keep their own loops,
// because a pass and a probe are driven by what changed as much as by the clock.
package job

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/takt/internal/server/telemetry"
)

// The name this package's telemetry is recorded under, which describes the code
// declaring it rather than whatever assembles the server.
const scope = "github.com/dsb-labs/takt/internal/server/job"

type (
	// The Job type describes one piece of work and how often it is done.
	Job struct {
		// What the work is called, in logs, spans and metrics.
		Name string
		// How often the work is done. Zero means the job is not run at all, which
		// is how a job an operator has turned off is expressed.
		Interval time.Duration
		// The work itself. A failure is logged and the job runs again on its next
		// tick, so it is expected to be safe to repeat.
		Run func(ctx context.Context) error
	}

	// The Runner type runs jobs on their intervals.
	Runner struct {
		logger      *slog.Logger
		instruments instruments
		tracer      trace.Tracer
		jobs        []Job
	}

	// The Config type contains fields used to construct a Runner.
	Config struct {
		// The logger failures are reported to.
		Logger *slog.Logger
		// The provider the runner's instruments are created from. May be nil, in
		// which case nothing is recorded.
		MeterProvider metric.MeterProvider
		// The provider runs are traced from. May be nil, in which case nothing is
		// traced.
		TracerProvider trace.TracerProvider
		// The jobs to run.
		Jobs []Job
	}
)

// New returns a Runner ready to run its jobs.
func New(config Config) *Runner {
	return &Runner{
		logger:      config.Logger.With("component", "job"),
		instruments: newInstruments(telemetry.Meter(config.MeterProvider, scope)),
		tracer:      telemetry.Tracer(config.TracerProvider, scope),
		jobs:        config.Jobs,
	}
}

// Run the jobs until the context is cancelled. Each runs once at the start and
// then on its own interval, independently of the others, so a slow one holds up
// nothing but itself.
//
// Once at the start as well as on the tick, so that a server which was down for
// longer than an interval does not carry what the job would have dealt with in the
// meantime until its first tick.
func (r *Runner) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	for _, j := range r.jobs {
		if j.Interval <= 0 {
			r.logger.With("job", j.Name).Debug("job is not scheduled")

			continue
		}

		g.Go(func() error {
			ticker := time.NewTicker(j.Interval)
			defer ticker.Stop()

			r.run(ctx, j)

			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					r.run(ctx, j)
				}
			}
		})
	}

	return g.Wait()
}

// run the job once, recording how it went.
func (r *Runner) run(ctx context.Context, j Job) {
	ctx, span := r.tracer.Start(ctx, "job.run", trace.WithAttributes(
		attribute.String("takt.job", j.Name),
	))
	defer span.End()

	started := time.Now()

	err := j.Run(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		r.logger.With("job", j.Name, "error", err).Warn("job failed")
	}

	r.instruments.duration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("job", j.Name),
		telemetry.OutcomeOf(err).Attribute(),
	))
}
