package telemetry_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"

	"github.com/dsb-labs/takt/internal/server/telemetry"
)

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("exports nothing by default", func(t *testing.T) {
		ctx := t.Context()

		tel, err := telemetry.New(ctx, telemetry.Config{})
		require.NoError(t, err)

		assert.Nil(t, tel.LogHandler())

		_, span := tel.TracerProvider().Tracer("test").Start(ctx, "test")
		defer span.End()

		assert.False(t, span.IsRecording())
		assert.NoError(t, tel.Shutdown(ctx))
	})

	t.Run("gathers recorded metrics", func(t *testing.T) {
		ctx := t.Context()

		tel, err := telemetry.New(ctx, telemetry.Config{})
		require.NoError(t, err)

		counter, err := tel.MeterProvider().Meter("test").Int64Counter("takt.test")
		require.NoError(t, err)
		counter.Add(ctx, 1)

		families, err := tel.Gatherer().Gather()
		require.NoError(t, err)

		found := false
		for _, family := range families {
			if strings.HasPrefix(family.GetName(), "takt_test") {
				found = true
			}
		}

		assert.True(t, found, "expected a gathered family named for the instrument")
	})

	t.Run("collects runtime metrics without being asked", func(t *testing.T) {
		tel, err := telemetry.New(t.Context(), telemetry.Config{})
		require.NoError(t, err)

		families, err := tel.Gatherer().Gather()
		require.NoError(t, err)

		found := false
		for _, family := range families {
			if strings.HasPrefix(family.GetName(), "go_") {
				found = true
			}
		}

		assert.True(t, found, "expected the runtime's own metrics on the scrape")
	})

	t.Run("rebuckets the database histogram", func(t *testing.T) {
		ctx := t.Context()

		tel, err := telemetry.New(ctx, telemetry.Config{})
		require.NoError(t, err)

		// Recording under otelsql's instrument name is enough to exercise the
		// view: the boundaries come from the provider, not the caller.
		histogram, err := tel.MeterProvider().Meter("test").Float64Histogram("db.client.operation.duration")
		require.NoError(t, err)
		histogram.Record(ctx, 0.002)

		families, err := tel.Gatherer().Gather()
		require.NoError(t, err)

		found := false
		for _, family := range families {
			if !strings.HasPrefix(family.GetName(), "db_client_operation_duration") {
				continue
			}

			found = true

			buckets := family.GetMetric()[0].GetHistogram().GetBucket()
			require.NotEmpty(t, buckets)
			assert.InDelta(t, telemetry.FastBoundaries[0], buckets[0].GetUpperBound(), 0)
		}

		assert.True(t, found, "expected the database histogram on the scrape")
	})

	t.Run("carries the host name on every signal", func(t *testing.T) {
		ctx := t.Context()

		var buffer bytes.Buffer
		exporter, err := stdouttrace.New(stdouttrace.WithWriter(&buffer))
		require.NoError(t, err)

		tel, err := telemetry.New(ctx, telemetry.Config{SpanExporter: exporter})
		require.NoError(t, err)

		_, span := tel.TracerProvider().Tracer("test").Start(ctx, "test-span")
		span.End()

		require.NoError(t, tel.Shutdown(ctx))

		// The resource travels with every exported span, which is what tells
		// one node's telemetry from another's at a shared collector.
		assert.Contains(t, buffer.String(), "host.name")
	})

	t.Run("carries spans to an injected exporter", func(t *testing.T) {
		ctx := t.Context()

		var buffer bytes.Buffer
		exporter, err := stdouttrace.New(stdouttrace.WithWriter(&buffer))
		require.NoError(t, err)

		tel, err := telemetry.New(ctx, telemetry.Config{SpanExporter: exporter})
		require.NoError(t, err)

		_, span := tel.TracerProvider().Tracer("test").Start(ctx, "test-span")
		span.End()

		// Spans are batched, so nothing reaches the exporter until the flush
		// that shutdown performs.
		require.NoError(t, tel.Shutdown(ctx))
		assert.Contains(t, buffer.String(), "test-span")
	})

	t.Run("carries logs to an injected exporter", func(t *testing.T) {
		ctx := t.Context()

		var buffer bytes.Buffer
		exporter, err := stdoutlog.New(stdoutlog.WithWriter(&buffer))
		require.NoError(t, err)

		tel, err := telemetry.New(ctx, telemetry.Config{LogExporter: exporter})
		require.NoError(t, err)
		require.NotNil(t, tel.LogHandler())

		slog.New(tel.LogHandler()).Info("hello from the test")

		require.NoError(t, tel.Shutdown(ctx))
		assert.Contains(t, buffer.String(), "hello from the test")
	})

	t.Run("rejects an unparseable endpoint", func(t *testing.T) {
		_, err := telemetry.New(t.Context(), telemetry.Config{Endpoint: "://not-a-url"})
		assert.Error(t, err)
	})
}
