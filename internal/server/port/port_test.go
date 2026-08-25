package port_test

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/dsb-labs/orca/internal/server/port"
)

func TestAllocator_RegisterMetrics(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	allocator := port.New(port.Config{Min: 20000, Max: 20009})

	err := allocator.RegisterMetrics(meter, func(context.Context) ([]int, error) {
		return []int{20001, 20004, 20007}, nil
	})
	require.NoError(t, err)

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &collected))

	assert.EqualValues(t, 10, gaugeValue(t, collected, "orca.ports.capacity"))
	assert.EqualValues(t, 3, gaugeValue(t, collected, "orca.ports.used"))
}

// gaugeValue returns the single data point of the named gauge, failing the test
// when it was never recorded.
func gaugeValue(t *testing.T, collected metricdata.ResourceMetrics, name string) int64 {
	t.Helper()

	for _, scope := range collected.ScopeMetrics {
		for _, recorded := range scope.Metrics {
			if recorded.Name != name {
				continue
			}

			gauge, ok := recorded.Data.(metricdata.Gauge[int64])
			require.True(t, ok)
			require.Len(t, gauge.DataPoints, 1)

			return gauge.DataPoints[0].Value
		}
	}

	t.Fatalf("no metric named %s was recorded", name)

	return 0
}

func TestAllocator_Allocate(t *testing.T) {
	t.Parallel()

	t.Run("returns a port from the range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29310, Max: 29320})

		got, err := allocator.Allocate(nil)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, got, 29310)
		assert.LessOrEqual(t, got, 29320)
	})

	t.Run("skips ports already allocated to a workload", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29330, Max: 29332})

		// Only one port in the range is unclaimed, so the allocator has to find it
		// wherever it starts looking.
		got, err := allocator.Allocate([]int{29330, 29332})
		require.NoError(t, err)
		assert.Equal(t, 29331, got)
	})

	t.Run("skips a port something on the host is listening on", func(t *testing.T) {
		// A listener orca knows nothing about is exactly the case the bind check
		// exists for: the database has no record of it, so only trying the port
		// reveals that it is unusable. Narrowing the range to two ports and holding
		// one leaves exactly one answer.
		listener, err := net.Listen("tcp", "127.0.0.1:29350")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, listener.Close()) })

		allocator := port.New(port.Config{Min: 29350, Max: 29351})

		got, err := allocator.Allocate(nil)
		require.NoError(t, err)
		assert.Equal(t, 29351, got)
	})

	t.Run("spreads allocations across the range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29400, Max: 29500})

		// Concurrent callers each read the same set of taken ports, so an allocator
		// that always chose the lowest free one would have them all pick the same
		// port and all but one lose the race to claim it. Spreading the starting
		// point is what keeps that contention rare.
		seen := make(map[int]struct{})
		for range 20 {
			got, err := allocator.Allocate(nil)
			require.NoError(t, err)

			seen[got] = struct{}{}
		}

		assert.Greater(t, len(seen), 1, "every allocation chose the same port")
	})

	t.Run("reports an exhausted range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29370, Max: 29372})

		_, err := allocator.Allocate([]int{29370, 29371, 29372})
		assert.ErrorIs(t, err, port.ErrRangeExhausted)
	})

	t.Run("allocates a single-port range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29380, Max: 29380})

		got, err := allocator.Allocate(nil)
		require.NoError(t, err)
		assert.Equal(t, 29380, got)
	})
}

func TestAvailable(t *testing.T) {
	t.Parallel()

	t.Run("reports a free port as available", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		_, p, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		require.NoError(t, listener.Close())

		free, err := strconv.Atoi(p)
		require.NoError(t, err)

		assert.True(t, port.Available(free))
	})

	t.Run("reports a bound port as unavailable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, listener.Close()) })

		_, p, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)

		bound, err := strconv.Atoi(p)
		require.NoError(t, err)

		assert.False(t, port.Available(bound))
	})
}
