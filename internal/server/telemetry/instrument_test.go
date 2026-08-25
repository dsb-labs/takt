package telemetry_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/dsb-labs/orca/internal/server/telemetry"
)

func TestOutcomeOf(t *testing.T) {
	t.Parallel()

	assert.Equal(t, telemetry.OutcomeOK, telemetry.OutcomeOf(nil))
	assert.Equal(t, telemetry.OutcomeError, telemetry.OutcomeOf(errors.New("broken")))
}

func TestOutcome_Attribute(t *testing.T) {
	t.Parallel()

	assert.Equal(t, attribute.String("outcome", "ok"), telemetry.OutcomeOK.Attribute())
}

func TestCounter(t *testing.T) {
	t.Parallel()

	t.Run("records against a real meter", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

		counter := telemetry.Counter(meter, "orca.test", "A counter the test builds.", "{thing}")
		counter.Add(t.Context(), 2)

		var collected metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(t.Context(), &collected))

		require.Len(t, collected.ScopeMetrics, 1)
		require.Len(t, collected.ScopeMetrics[0].Metrics, 1)
		assert.Equal(t, "orca.test", collected.ScopeMetrics[0].Metrics[0].Name)
	})

	t.Run("records nothing without a meter", func(t *testing.T) {
		// The point is that a caller never guards: a nil meter yields an
		// instrument that is safe to record into.
		counter := telemetry.Counter(nil, "orca.test", "A counter the test builds.", "{thing}")
		counter.Add(t.Context(), 1)
	})

	t.Run("survives an instrument the meter refuses", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

		// An empty name is refused by the SDK, which must cost the metric
		// rather than the caller.
		counter := telemetry.Counter(meter, "", "A counter the test builds.", "{thing}")
		counter.Add(t.Context(), 1)
	})
}

func TestTracer(t *testing.T) {
	t.Parallel()

	_, span := telemetry.Tracer(nil).Start(t.Context(), "test")
	defer span.End()

	assert.False(t, span.IsRecording())
}
