package port_test

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/dsb-labs/orca/internal/server/port"
)

func TestAllocator_RegisterMetrics(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

	allocator := port.New(port.Config{Min: 20000, Max: 20009})

	err := allocator.RegisterMetrics(meter, func(context.Context) (map[port.Protocol]int, error) {
		return map[port.Protocol]int{port.ProtocolTCP: 3, port.ProtocolUDP: 1}, nil
	})
	require.NoError(t, err)

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &collected))

	assert.EqualValues(t, 10, gaugeValue(t, collected, "orca.ports.capacity"))
	assert.EqualValues(t, 3, protocolGaugeValue(t, collected, "orca.ports.used", port.ProtocolTCP))
	assert.EqualValues(t, 1, protocolGaugeValue(t, collected, "orca.ports.used", port.ProtocolUDP))
}

// gaugeValue returns the single data point of the named gauge, failing the test
// when it was never recorded.
func gaugeValue(t *testing.T, collected metricdata.ResourceMetrics, name string) int64 {
	t.Helper()

	points := gaugePoints(t, collected, name)
	require.Len(t, points, 1)

	return points[0].Value
}

// protocolGaugeValue returns the data point of the named gauge carrying the given
// protocol, failing the test when it was never recorded.
func protocolGaugeValue(t *testing.T, collected metricdata.ResourceMetrics, name string, protocol port.Protocol) int64 {
	t.Helper()

	for _, point := range gaugePoints(t, collected, name) {
		if value, ok := point.Attributes.Value(attribute.Key("protocol")); ok && value.AsString() == string(protocol) {
			return point.Value
		}
	}

	t.Fatalf("no %s data point of metric %s was recorded", protocol, name)

	return 0
}

// gaugePoints returns the data points of the named gauge, failing the test when it
// was never recorded.
func gaugePoints(t *testing.T, collected metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	t.Helper()

	for _, scope := range collected.ScopeMetrics {
		for _, recorded := range scope.Metrics {
			if recorded.Name != name {
				continue
			}

			gauge, ok := recorded.Data.(metricdata.Gauge[int64])
			require.True(t, ok)

			return gauge.DataPoints
		}
	}

	t.Fatalf("no metric named %s was recorded", name)

	return nil
}

func TestAllocator_Allocate(t *testing.T) {
	t.Parallel()

	t.Run("returns a port from the range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29310, Max: 29320})

		got, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP}, nil)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, got, 29310)
		assert.LessOrEqual(t, got, 29320)
	})

	t.Run("skips ports already allocated to a workload", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29330, Max: 29332})

		// Only one port in the range is unclaimed, so the allocator has to find it
		// wherever it starts looking.
		got, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP}, map[port.Protocol][]int{
			port.ProtocolTCP: {29330, 29332},
		})
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

		got, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP}, nil)
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
			got, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP}, nil)
			require.NoError(t, err)

			seen[got] = struct{}{}
		}

		assert.Greater(t, len(seen), 1, "every allocation chose the same port")
	})

	t.Run("reports an exhausted range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29370, Max: 29372})

		_, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP}, map[port.Protocol][]int{
			port.ProtocolTCP: {29370, 29371, 29372},
		})
		assert.ErrorIs(t, err, port.ErrRangeExhausted)
	})

	t.Run("allocates a single-port range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29380, Max: 29380})

		got, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP}, nil)
		require.NoError(t, err)
		assert.Equal(t, 29380, got)
	})

	t.Run("ignores an allocation on the other protocol", func(t *testing.T) {
		// The two are separate address spaces, so a port taken over TCP says nothing
		// about the same number over UDP. An allocator that shared one set of taken
		// ports would refuse a port that is genuinely free.
		allocator := port.New(port.Config{Min: 29390, Max: 29390})

		got, err := allocator.Allocate([]port.Protocol{port.ProtocolUDP}, map[port.Protocol][]int{
			port.ProtocolTCP: {29390},
		})
		require.NoError(t, err)
		assert.Equal(t, 29390, got)
	})

	t.Run("skips a port bound over udp", func(t *testing.T) {
		conn, err := net.ListenPacket("udp", "127.0.0.1:29360")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })

		allocator := port.New(port.Config{Min: 29360, Max: 29361})

		got, err := allocator.Allocate([]port.Protocol{port.ProtocolUDP}, nil)
		require.NoError(t, err)
		assert.Equal(t, 29361, got)
	})

	t.Run("returns a port free on both protocols", func(t *testing.T) {
		// A workload publishing one port over both protocols is given the same host
		// port for each, so the allocation has to satisfy the two at once.
		allocator := port.New(port.Config{Min: 29340, Max: 29342})

		got, err := allocator.Allocate([]port.Protocol{port.ProtocolTCP, port.ProtocolUDP}, map[port.Protocol][]int{
			port.ProtocolTCP: {29340},
			port.ProtocolUDP: {29342},
		})
		require.NoError(t, err)
		assert.Equal(t, 29341, got)
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

		assert.True(t, port.Available(port.ProtocolTCP, free))
	})

	t.Run("reports a bound port as unavailable", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, listener.Close()) })

		_, p, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)

		bound, err := strconv.Atoi(p)
		require.NoError(t, err)

		assert.False(t, port.Available(port.ProtocolTCP, bound))
	})

	t.Run("reports a port bound over udp as unavailable", func(t *testing.T) {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })

		_, p, err := net.SplitHostPort(conn.LocalAddr().String())
		require.NoError(t, err)

		bound, err := strconv.Atoi(p)
		require.NoError(t, err)

		assert.False(t, port.Available(port.ProtocolUDP, bound))
	})

	t.Run("reports a port bound over udp as available over tcp", func(t *testing.T) {
		// The two spaces are unrelated, so probing UDP with a TCP listen would report
		// a taken port as free and a free one as taken.
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })

		_, p, err := net.SplitHostPort(conn.LocalAddr().String())
		require.NoError(t, err)

		bound, err := strconv.Atoi(p)
		require.NoError(t, err)

		assert.True(t, port.Available(port.ProtocolTCP, bound))
	})
}
