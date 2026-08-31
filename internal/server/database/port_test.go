package database_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
)

func TestPortRepository_Claim(t *testing.T) {
	t.Parallel()

	t.Run("stores the ports allocated to a workload", func(t *testing.T) {
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Container: 9090, Host: 4141, Protocol: "tcp"},
		}))

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, 8080, got[0].Container)
		assert.Equal(t, 20000, got[0].Host)
		assert.True(t, got[0].Dynamic)

		assert.Equal(t, 9090, got[1].Container)
		assert.Equal(t, 4141, got[1].Host)
		assert.False(t, got[1].Dynamic)
	})

	t.Run("stores the name a port was given", func(t *testing.T) {
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Name: "http", Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Container: 9090, Host: 4141, Protocol: "tcp"},
		}))

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "http", got[0].Name)
		assert.Empty(t, got[1].Name)
	})

	t.Run("stores the same container port for two instances", func(t *testing.T) {
		// Each instance of a workload publishes the same container port on a host
		// port of its own, which is two allocations distinguished by the instance.
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Instance: 0, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Instance: 1, Container: 8080, Host: 20001, Protocol: "tcp", Dynamic: true},
		}))

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, 0, got[0].Instance)
		assert.Equal(t, 20000, got[0].Host)
		assert.Equal(t, 1, got[1].Instance)
		assert.Equal(t, 20001, got[1].Host)
	})

	t.Run("stores one port on both protocols", func(t *testing.T) {
		// A workload speaking DNS publishes 53 over both, which is two allocations of
		// the same number rather than one allocation named twice.
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 53, Host: 20000, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Container: 53, Host: 20000, Protocol: "udp", Dynamic: true},
		}))

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "tcp", got[0].Protocol)
		assert.Equal(t, "udp", got[1].Protocol)
		assert.Equal(t, 20000, got[0].Host)
		assert.Equal(t, 20000, got[1].Host)
	})

	t.Run("replaces the previous allocation wholesale", func(t *testing.T) {
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Container: 9090, Host: 20001, Protocol: "tcp", Dynamic: true},
		}))

		// Re-claiming with one port has to release the other, which is how a port
		// removed from a specification gives its host port back.
		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
		}))

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, 8080, got[0].Container)

		_, held, err := ports.HolderOf(ctx, 20001, "tcp")
		require.NoError(t, err)
		assert.False(t, held)
	})

	t.Run("rejects a host port held by another workload", func(t *testing.T) {
		ports, example, other := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, other, []database.Port{
			{WorkloadID: other, Container: 8080, Host: 4141, Protocol: "tcp"},
		}))

		err := ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 4141, Protocol: "tcp"},
		})
		assert.ErrorIs(t, err, database.ErrHostPortTaken)

		// The rejected claim must not have disturbed the holder's allocation.
		got, err := ports.List(ctx, other)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, 4141, got[0].Host)
	})

	t.Run("leaves the previous allocation intact when a claim fails", func(t *testing.T) {
		ports, example, other := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, other, []database.Port{
			{WorkloadID: other, Container: 8080, Host: 4141, Protocol: "tcp"},
		}))
		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
		}))

		// The second port collides, so the whole claim has to roll back rather than
		// leaving the workload holding only the first.
		err := ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20002, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Container: 9090, Host: 4141, Protocol: "tcp"},
		})
		assert.ErrorIs(t, err, database.ErrHostPortTaken)

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, 20000, got[0].Host)
	})
}

func TestPortRepository_ListAll(t *testing.T) {
	t.Parallel()

	t.Run("returns every workload's ports in one read", func(t *testing.T) {
		ports, example, other := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
			{WorkloadID: example, Container: 9090, Host: 20001, Protocol: "tcp", Dynamic: true},
		}))
		require.NoError(t, ports.Claim(ctx, other, []database.Port{
			{WorkloadID: other, Container: 8080, Host: 4141, Protocol: "tcp"},
		}))

		// Listing workloads reads this once rather than once per workload, which is
		// what keeps a list of many workloads from costing a query each.
		got, err := ports.ListAll(ctx)
		require.NoError(t, err)
		require.Len(t, got, 2)

		require.Len(t, got[example], 2)
		assert.Equal(t, 8080, got[example][0].Container)
		assert.Equal(t, 9090, got[example][1].Container)

		require.Len(t, got[other], 1)
		assert.Equal(t, 4141, got[other][0].Host)
	})

	t.Run("returns nothing when no ports are allocated", func(t *testing.T) {
		ports, _, _ := newTestPorts(t)

		got, err := ports.ListAll(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestPortRepository_HolderOf(t *testing.T) {
	t.Parallel()

	t.Run("names the workload holding a port", func(t *testing.T) {
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 4141, Protocol: "tcp"},
		}))

		holder, held, err := ports.HolderOf(ctx, 4141, "tcp")
		require.NoError(t, err)

		assert.True(t, held)
		assert.Equal(t, "example", holder)
	})

	t.Run("reports an unheld port", func(t *testing.T) {
		ports, _, _ := newTestPorts(t)

		_, held, err := ports.HolderOf(t.Context(), 4141, "tcp")
		require.NoError(t, err)
		assert.False(t, held)
	})
}

func TestPortRepository_Allocated(t *testing.T) {
	t.Parallel()

	t.Run("returns every allocated port", func(t *testing.T) {
		ports, example, other := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20001, Protocol: "tcp", Dynamic: true},
		}))
		require.NoError(t, ports.Claim(ctx, other, []database.Port{
			{WorkloadID: other, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
		}))

		got, err := ports.Allocated(ctx)
		require.NoError(t, err)
		assert.Equal(t, map[string][]int{"tcp": {20000, 20001}}, got)
	})

	t.Run("keys the allocations by protocol", func(t *testing.T) {
		// The two are separate address spaces, so a flat list would have 20000/tcp
		// rule out 20000/udp and the allocator would refuse a port that is free.
		ports, example, other := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 53, Host: 20000, Protocol: "tcp", Dynamic: true},
		}))
		require.NoError(t, ports.Claim(ctx, other, []database.Port{
			{WorkloadID: other, Container: 53, Host: 20000, Protocol: "udp", Dynamic: true},
		}))

		got, err := ports.Allocated(ctx)
		require.NoError(t, err)
		assert.Equal(t, map[string][]int{"tcp": {20000}, "udp": {20000}}, got)
	})
}

func TestPortRepository_Release(t *testing.T) {
	t.Parallel()

	t.Run("gives a workload's ports back", func(t *testing.T) {
		ports, example, _ := newTestPorts(t)
		ctx := t.Context()

		require.NoError(t, ports.Claim(ctx, example, []database.Port{
			{WorkloadID: example, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
		}))

		require.NoError(t, ports.Release(ctx, example))

		got, err := ports.List(ctx, example)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// TestWorkloadRepository_Delete_ReleasesPorts covers the two domains together: a
// deleted workload must not leave a host port allocated, since nothing would ever
// reclaim it.
func TestWorkloadRepository_Delete_ReleasesPorts(t *testing.T) {
	t.Parallel()

	db := newTestDatabase(t)
	workloads, ports := database.NewWorkloadRepository(db), database.NewPortRepository(db)
	ctx := t.Context()

	stored, _, err := workloads.Upsert(ctx, database.Workload{
		Name:     "example",
		Runtime:  "container",
		Spec:     []byte(`{}`),
		SpecHash: "hash-one",
	})
	require.NoError(t, err)

	require.NoError(t, ports.Claim(ctx, stored.ID, []database.Port{
		{WorkloadID: stored.ID, Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true},
	}))

	require.NoError(t, workloads.Delete(ctx, "example"))

	allocated, err := ports.Allocated(ctx)
	require.NoError(t, err)
	assert.Empty(t, allocated)
}

// newTestPorts returns a port repository alongside the identifiers of two stored
// workloads, since an allocation has to belong to a workload that exists.
func newTestPorts(t *testing.T) (*database.PortRepository, string, string) {
	t.Helper()

	db := newTestDatabase(t)
	workloads := database.NewWorkloadRepository(db)

	ids := make([]string, 0, 2)
	for _, name := range []string{"example", "other"} {
		stored, _, err := workloads.Upsert(t.Context(), database.Workload{
			Name:     name,
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash-" + name,
		})
		require.NoError(t, err)

		ids = append(ids, stored.ID)
	}

	return database.NewPortRepository(db), ids[0], ids[1]
}

func newTestDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := database.Open(t.Context(), database.Config{
		Logger: newTestLogger(t),
		Path:   filepath.Join(t.TempDir(), "test.db"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	return db
}
