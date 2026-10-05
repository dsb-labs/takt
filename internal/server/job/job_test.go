package job_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/dsb-labs/takt/internal/server/job"
)

func TestRunner_Run(t *testing.T) {
	t.Parallel()

	t.Run("runs a job once at the start and then on its interval", func(t *testing.T) {
		var runs atomic.Int32

		runner := job.New(job.Config{
			Logger: newTestLogger(t),
			Jobs: []job.Job{{
				Name:     "count",
				Interval: 10 * time.Millisecond,
				Run: func(context.Context) error {
					runs.Add(1)

					return nil
				},
			}},
		})

		stop := run(t, runner)
		defer stop()

		require.Eventually(t, func() bool { return runs.Load() >= 3 }, time.Second, time.Millisecond)
	})

	t.Run("keeps running a job that fails", func(t *testing.T) {
		var runs atomic.Int32

		runner := job.New(job.Config{
			Logger: newTestLogger(t),
			Jobs: []job.Job{{
				Name:     "failing",
				Interval: 10 * time.Millisecond,
				Run: func(context.Context) error {
					runs.Add(1)

					return errors.New("registry unreachable")
				},
			}},
		})

		stop := run(t, runner)
		defer stop()

		require.Eventually(t, func() bool { return runs.Load() >= 3 }, time.Second, time.Millisecond)
	})

	t.Run("does not run a job with no interval", func(t *testing.T) {
		var scheduled, unscheduled atomic.Int32

		runner := job.New(job.Config{
			Logger: newTestLogger(t),
			Jobs: []job.Job{
				{
					Name:     "off",
					Interval: 0,
					Run: func(context.Context) error {
						unscheduled.Add(1)

						return nil
					},
				},
				{
					Name:     "on",
					Interval: 10 * time.Millisecond,
					Run: func(context.Context) error {
						scheduled.Add(1)

						return nil
					},
				},
			},
		})

		stop := run(t, runner)
		defer stop()

		require.Eventually(t, func() bool { return scheduled.Load() >= 3 }, time.Second, time.Millisecond)
		assert.Zero(t, unscheduled.Load(), "a job with no interval ran")
	})

	t.Run("returns once the context is cancelled", func(t *testing.T) {
		runner := job.New(job.Config{
			Logger: newTestLogger(t),
			Jobs: []job.Job{{
				Name:     "idle",
				Interval: time.Hour,
				Run:      func(context.Context) error { return nil },
			}},
		})

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		require.NoError(t, runner.Run(ctx))
	})

	t.Run("hands the job a context the cancellation reaches", func(t *testing.T) {
		started := make(chan struct{})
		ended := make(chan error, 1)

		runner := job.New(job.Config{
			Logger: newTestLogger(t),
			Jobs: []job.Job{{
				Name:     "blocking",
				Interval: time.Hour,
				Run: func(ctx context.Context) error {
					close(started)
					<-ctx.Done()
					ended <- ctx.Err()

					return nil
				},
			}},
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- runner.Run(ctx) }()

		<-started
		cancel()
		require.NoError(t, <-done)
		assert.ErrorIs(t, <-ended, context.Canceled)
	})
}

func TestRunner_Metrics(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	runner := job.New(job.Config{
		Logger:        newTestLogger(t),
		MeterProvider: provider,
		Jobs: []job.Job{{
			Name:     "failing",
			Interval: time.Hour,
			Run: func(context.Context) error {
				return errors.New("registry unreachable")
			},
		}},
	})

	stop := run(t, runner)
	defer stop()

	// The duration is recorded after the job returns, so the job having run is
	// not the signal to wait on. The metric itself is.
	assert.Eventually(t, func() bool {
		var collected metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &collected); err != nil {
			return false
		}

		for _, scope := range collected.ScopeMetrics {
			for _, recorded := range scope.Metrics {
				if recorded.Name != "takt.job.duration" {
					continue
				}

				histogram, ok := recorded.Data.(metricdata.Histogram[float64])
				if !ok {
					return false
				}

				for _, point := range histogram.DataPoints {
					name, _ := point.Attributes.Value("job")
					outcome, _ := point.Attributes.Value("outcome")
					if name.AsString() == "failing" && outcome.AsString() == "error" && point.Count >= 1 {
						return true
					}
				}
			}
		}

		return false
	}, time.Second, time.Millisecond, "expected a recorded run duration for the failing job")
}

func TestRunner_Spans(t *testing.T) {
	t.Parallel()

	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))

	var runs atomic.Int32

	runner := job.New(job.Config{
		Logger:         newTestLogger(t),
		TracerProvider: provider,
		Jobs: []job.Job{{
			Name:     "failing",
			Interval: time.Hour,
			Run: func(context.Context) error {
				runs.Add(1)

				return errors.New("registry unreachable")
			},
		}},
	})

	stop := run(t, runner)
	defer stop()

	require.Eventually(t, func() bool { return runs.Load() >= 1 }, time.Second, time.Millisecond)

	require.Eventually(t, func() bool {
		return slices.ContainsFunc(spans.Ended(), func(span sdktrace.ReadOnlySpan) bool {
			if span.Name() != "job.run" || span.Status().Code != codes.Error {
				return false
			}

			return slices.ContainsFunc(span.Attributes(), func(kv attribute.KeyValue) bool {
				return kv.Key == "takt.job" && kv.Value.AsString() == "failing"
			})
		})
	}, time.Second, time.Millisecond, "expected an ended run span for the failing job")
}

// run the runner in the background, returning a function that stops it and
// requires it to have returned cleanly.
func run(t *testing.T, runner *job.Runner) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- runner.Run(ctx) }()

	return func() {
		cancel()
		require.NoError(t, <-done)
	}
}

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}
