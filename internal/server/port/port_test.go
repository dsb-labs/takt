package port_test

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/port"
)

func TestAllocator_Allocate(t *testing.T) {
	t.Parallel()

	t.Run("returns the lowest free port in the range", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29310, Max: 29320})

		got, err := allocator.Allocate(nil)
		require.NoError(t, err)

		// Ascending order means a given set of workloads tends to get the same
		// ports across restarts, which is what makes them predictable.
		assert.Equal(t, 29310, got)
	})

	t.Run("skips ports already allocated to a workload", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29330, Max: 29340})

		got, err := allocator.Allocate([]int{29330, 29331})
		require.NoError(t, err)
		assert.Equal(t, 29332, got)
	})

	t.Run("skips a port something on the host is listening on", func(t *testing.T) {
		allocator := port.New(port.Config{Min: 29350, Max: 29360})

		// A listener orca knows nothing about is exactly the case the bind check
		// exists for: the database has no record of it, so only trying the port
		// reveals that it is unusable.
		listener, err := net.Listen("tcp", "127.0.0.1:29350")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, listener.Close()) })

		got, err := allocator.Allocate(nil)
		require.NoError(t, err)
		assert.Equal(t, 29351, got)
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
