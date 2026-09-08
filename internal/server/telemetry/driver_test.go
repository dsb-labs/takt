package telemetry_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/telemetry"
)

func TestWrapDriver(t *testing.T) {
	t.Parallel()

	t.Run("traces each operation under its own name", func(t *testing.T) {
		ctx := t.Context()

		spans := tracetest.NewSpanRecorder()
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))

		d := NewMockDriver(t)
		d.EXPECT().Name().Return("stub")
		d.EXPECT().Start(mock.Anything, mock.Anything).Return("id", nil)
		d.EXPECT().Stop(mock.Anything, "id", "web").Return(nil)
		d.EXPECT().Discard(mock.Anything, "id", "web").Return(nil)
		d.EXPECT().StopInstance(mock.Anything, "id", "web", 1).Return(nil)
		d.EXPECT().DiscardInstance(mock.Anything, "id", "web", 1).Return(nil)
		d.EXPECT().Signal(mock.Anything, "id", "web", "HUP").Return(nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		wrapped := telemetry.WrapDriver(d, provider)

		_, err := wrapped.Start(ctx, driver.Workload{Name: "web", Instance: 1})
		require.NoError(t, err)
		require.NoError(t, wrapped.Stop(ctx, "id", "web"))
		require.NoError(t, wrapped.Discard(ctx, "id", "web"))
		require.NoError(t, wrapped.StopInstance(ctx, "id", "web", 1))
		require.NoError(t, wrapped.DiscardInstance(ctx, "id", "web", 1))
		require.NoError(t, wrapped.Signal(ctx, "id", "web", "HUP"))

		_, err = wrapped.Observe(ctx)
		require.NoError(t, err)

		names := make([]string, 0, len(spans.Ended()))
		for _, span := range spans.Ended() {
			names = append(names, span.Name())
			assert.Contains(t, span.Attributes(), attribute.String("takt.driver", "stub"))
		}

		assert.Equal(t, []string{
			"driver.start", "driver.stop", "driver.discard",
			"driver.stop", "driver.discard", "driver.signal", "driver.observe",
		}, names)
	})

	t.Run("records the error a call fails with", func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))

		d := NewMockDriver(t)
		d.EXPECT().Name().Return("stub")
		d.EXPECT().Stop(mock.Anything, "id", "web").Return(errors.New("broken"))

		wrapped := telemetry.WrapDriver(d, provider)
		assert.Error(t, wrapped.Stop(t.Context(), "id", "web"))

		ended := spans.Ended()
		require.Len(t, ended, 1)
		assert.Equal(t, codes.Error, ended[0].Status().Code)
		assert.NotEmpty(t, ended[0].Events(), "expected the error recorded as an event")
	})

	t.Run("records nothing without a provider", func(t *testing.T) {
		d := NewMockDriver(t)
		d.EXPECT().Name().Return("stub")
		d.EXPECT().Stop(mock.Anything, "id", "web").Return(nil)

		wrapped := telemetry.WrapDriver(d, nil)
		assert.NoError(t, wrapped.Stop(t.Context(), "id", "web"))
	})
}
