package database_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/event"
)

func TestWorkloadEventRepository_Record(t *testing.T) {
	t.Parallel()

	t.Run("records an event against a workload", func(t *testing.T) {
		events, _, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		assert.Equal(t, event.ImagePulling, stored[0].Reason)
		assert.JSONEq(t, `{"ref":"alpine:3"}`, string(stored[0].Data))
		assert.Equal(t, 1, stored[0].Count)
		assert.Equal(t, stored[0].FirstSeen, stored[0].LastSeen)
	})

	t.Run("coalesces a repeat of the same reason and data", func(t *testing.T) {
		events, _, _ := newTestEventRepository(t)
		ctx := t.Context()

		for range 3 {
			require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))
		}

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		assert.Equal(t, 3, stored[0].Count)
		assert.True(t, stored[0].LastSeen.After(stored[0].FirstSeen) ||
			stored[0].LastSeen.Equal(stored[0].FirstSeen))
	})

	t.Run("keeps events with the same reason but different data apart", func(t *testing.T) {
		events, _, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))
		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:4"}`)))

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		assert.Len(t, stored, 2)
	})

	t.Run("starts a new episode when the previous sighting has gone stale", func(t *testing.T) {
		events, db, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))

		first, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, first, 1)

		backdate(t, db, first[0].ID, time.Now().UTC().Add(-30*time.Minute))

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))

		second, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, second, 1)

		// The count restarts and the first seen time moves forward, so the row
		// describes the sighting that just happened rather than spanning both.
		assert.Equal(t, 1, second[0].Count)
		assert.True(t, second[0].FirstSeen.After(first[0].FirstSeen))
	})

	t.Run("continues the episode when the previous sighting is recent", func(t *testing.T) {
		events, db, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))

		first, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, first, 1)

		backdate(t, db, first[0].ID, time.Now().UTC().Add(-5*time.Minute))

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))

		second, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, second, 1)

		assert.Equal(t, 2, second[0].Count)
	})

	t.Run("records nothing for a workload that does not exist", func(t *testing.T) {
		events, _, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "missing", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))

		stored, err := events.List(ctx, "missing", time.Time{}, 10)
		require.NoError(t, err)
		assert.Empty(t, stored)
	})

	t.Run("keeps the newest events when the cap is passed", func(t *testing.T) {
		_, db, _ := newTestEventRepository(t)
		ctx := t.Context()

		events := database.NewWorkloadEventRepository(db, 5)

		// Each record carries distinct data, so none of them coalesce and the cap
		// is what decides how many survive.
		for i := range 20 {
			require.NoError(t, events.Record(ctx, "example", event.InstanceStarted,
				[]byte(fmt.Sprintf(`{"instance":%d}`, i))))
		}

		stored, err := events.List(ctx, "example", time.Time{}, 500)
		require.NoError(t, err)
		assert.Len(t, stored, 5)

		// The most recent write survives the prune, which is the end that matters.
		assert.JSONEq(t, `{"instance":19}`, string(stored[0].Data))
	})
}

func TestWorkloadEventRepository_List(t *testing.T) {
	t.Parallel()

	t.Run("returns the most recently seen event first", func(t *testing.T) {
		events, db, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{}`)))

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		backdate(t, db, stored[0].ID, time.Now().UTC().Add(-time.Hour))

		require.NoError(t, events.Record(ctx, "example", event.InstanceStarted, []byte(`{}`)))

		ordered, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, ordered, 2)

		assert.Equal(t, event.InstanceStarted, ordered[0].Reason)
		assert.Equal(t, event.ImagePulling, ordered[1].Reason)
	})

	t.Run("returns no more than the requested number of events", func(t *testing.T) {
		events, _, _ := newTestEventRepository(t)
		ctx := t.Context()

		for i := range 5 {
			require.NoError(t, events.Record(ctx, "example", event.InstanceStarted,
				[]byte(fmt.Sprintf(`{"instance":%d}`, i))))
		}

		stored, err := events.List(ctx, "example", time.Time{}, 2)
		require.NoError(t, err)
		assert.Len(t, stored, 2)
	})

	t.Run("drops the events last seen at or before the given time", func(t *testing.T) {
		events, db, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{}`)))

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		backdate(t, db, stored[0].ID, time.Now().UTC().Add(-time.Hour))

		require.NoError(t, events.Record(ctx, "example", event.InstanceStarted, []byte(`{}`)))

		recent, err := events.List(ctx, "example", time.Now().UTC().Add(-time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, recent, 1)

		assert.Equal(t, event.InstanceStarted, recent[0].Reason)
	})

	t.Run("returns an event again once a repeat has moved it", func(t *testing.T) {
		events, _, _ := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{}`)))

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		assert.Empty(t, mustList(t, events, stored[0].LastSeen))

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{}`)))

		repeated := mustList(t, events, stored[0].LastSeen)
		require.Len(t, repeated, 1)

		assert.Equal(t, 2, repeated[0].Count)
	})

	t.Run("returns nothing once the workload is deleted", func(t *testing.T) {
		events, _, workloads := newTestEventRepository(t)
		ctx := t.Context()

		require.NoError(t, events.Record(ctx, "example", event.ImagePulling, []byte(`{"ref":"alpine:3"}`)))
		require.NoError(t, workloads.Delete(ctx, "example"))

		stored, err := events.List(ctx, "example", time.Time{}, 10)
		require.NoError(t, err)
		assert.Empty(t, stored)
	})
}

func newTestEventRepository(t *testing.T) (*database.WorkloadEventRepository, *sql.DB, *database.WorkloadRepository) {
	t.Helper()

	db := newTestDatabase(t)
	workloads := database.NewWorkloadRepository(db)

	_, _, err := workloads.Upsert(t.Context(), database.Workload{
		Name:     "example",
		Runtime:  "container",
		Spec:     []byte(`{"name":"example"}`),
		SpecHash: "hash-one",
	}, 0)
	require.NoError(t, err)

	return database.NewWorkloadEventRepository(db, event.DefaultMaxEvents), db, workloads
}

// mustList reads the example workload's events last seen after the given time.
func mustList(t *testing.T, events *database.WorkloadEventRepository, since time.Time) []database.WorkloadEvent {
	t.Helper()

	stored, err := events.List(t.Context(), "example", since, 10)
	require.NoError(t, err)

	return stored
}

// backdate moves an event's last seen time into the past, which is how a test
// reaches a state that would otherwise take the staleness window to arrive at.
func backdate(t *testing.T, db *sql.DB, id string, at time.Time) {
	t.Helper()

	_, err := db.ExecContext(t.Context(),
		`UPDATE workload_event SET last_seen = ? WHERE id = ?`,
		at.Format("2006-01-02T15:04:05.000000000Z07:00"), id)
	require.NoError(t, err)
}
