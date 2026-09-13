package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/xid"

	"github.com/dsb-labs/takt/internal/server/event"
)

const (
	// How long a row may go untouched before a further sighting is treated as a
	// new episode rather than a continuation of the old one.
	//
	// Without this a cause that recurs days later would read as one episode with a
	// count of two, describing neither sighting. The window has to clear the
	// reconciler's restart backoff ceiling, two minutes, plus a reconcile
	// interval, so that a workload retrying slowly is not seen as having stopped.
	workloadEventGap = 15 * time.Minute
)

type (
	// The WorkloadEvent type represents something the server observed about a
	// workload while converging it.
	//
	// Repeated sightings of the same reason and data coalesce into one row rather
	// than accumulating, so the row carries a count and the two times that bound
	// it. It says a thing was true as of LastSeen, which is a claim about the past
	// rather than about the workload now.
	WorkloadEvent struct {
		// The identifier the server assigns to the event.
		ID string
		// The identifier of the workload the event was recorded against.
		WorkloadID string
		// Why the event was recorded, as a stable code rather than a sentence. The
		// prose an operator reads is rendered from this and Data when the event is
		// read, so the wording is not frozen into the row.
		Reason event.Reason
		// The canonical JSON encoding of the values the reason needs to be rendered,
		// such as the image reference a pull is fetching.
		Data []byte
		// How many times the event has been seen since FirstSeen.
		Count int
		// When the current episode of the event was first seen.
		FirstSeen time.Time
		// When the event was last seen.
		LastSeen time.Time
	}

	// The WorkloadEventRepository type provides persistence operations for the
	// workload event domain.
	WorkloadEventRepository struct {
		db        *sql.DB
		maxEvents int
	}
)

// NewWorkloadEventRepository returns a WorkloadEventRepository backed by the
// given database, keeping at most maxEvents events for any one workload.
func NewWorkloadEventRepository(db *sql.DB, maxEvents int) *WorkloadEventRepository {
	return &WorkloadEventRepository{db: db, maxEvents: maxEvents}
}

// Record stores an event against the named workload, coalescing it with an
// existing event carrying the same reason and data.
//
// Coalescing is what makes this safe to call from the reconciler, which runs a
// pass every few seconds and would otherwise write a row per pass for as long as
// a condition lasts. A repeat of an event already stored increments its count and
// moves its last seen time, which is one update to an indexed row.
//
// A workload that no longer exists records nothing and reports no error. The
// reconciler can be converging a workload the operator has just deleted, and a
// lost event is the right outcome there rather than a failed pass.
func (r *WorkloadEventRepository) Record(ctx context.Context, name string, reason event.Reason, data []byte) error {
	const q = `
		INSERT INTO workload_event (id, workload_id, reason, data, count, first_seen, last_seen)
		SELECT ?, w.id, ?, jsonb(?), 1, ?, ?
		FROM workload w
		WHERE w.name = ?
		ON CONFLICT (workload_id, reason, data) DO UPDATE SET
			count      = CASE WHEN workload_event.last_seen < ? THEN 1 ELSE workload_event.count + 1 END,
			first_seen = CASE WHEN workload_event.last_seen < ? THEN excluded.first_seen ELSE workload_event.first_seen END,
			last_seen  = excluded.last_seen
		RETURNING workload_id, count
	`

	now := time.Now().UTC()
	timestamp := formatTime(now)
	stale := formatTime(now.Add(-workloadEventGap))

	return transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		var (
			workloadID string
			count      int
		)

		err := tx.QueryRowContext(ctx, q, xid.New().String(), reason, string(data), timestamp, timestamp, name, stale, stale).
			Scan(&workloadID, &count)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("failed to record workload event: %w", err)
		}

		// A count of one means the row was inserted, or reset by the staleness rule
		// above, so the workload may now hold more events than it is allowed. A
		// coalesced sighting cannot have grown the table, which is what keeps the
		// steady state to a single statement. Pruning after a reset deletes nothing,
		// which is cheaper than a second statement to tell the two cases apart.
		if count > 1 {
			return nil
		}

		return prune(ctx, tx, workloadID, r.maxEvents)
	})
}

// List returns the events recorded against the named workload, most recently
// seen first, up to limit rows.
//
// A workload that does not exist has no events, and so reads as empty rather than
// as a failure. Callers that need to tell the two apart ask for the workload
// first.
func (r *WorkloadEventRepository) List(ctx context.Context, name string, limit int) ([]WorkloadEvent, error) {
	const q = `
		SELECT e.id, e.workload_id, e.reason, json(e.data), e.count, e.first_seen, e.last_seen
		FROM workload_event e
		JOIN workload w ON w.id = e.workload_id
		WHERE w.name = ?
		ORDER BY e.last_seen DESC
		LIMIT ?
	`

	rows, err := r.db.QueryContext(ctx, q, name, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query workload events: %w", err)
	}

	events := make([]WorkloadEvent, 0, limit)
	defer rows.Close()

	for rows.Next() {
		var (
			event               WorkloadEvent
			data                string
			firstSeen, lastSeen string
		)

		if err = rows.Scan(&event.ID, &event.WorkloadID, &event.Reason, &data, &event.Count, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("failed to scan workload event: %w", err)
		}

		event.Data = []byte(data)

		if event.FirstSeen, err = time.Parse(time.RFC3339Nano, firstSeen); err != nil {
			return nil, fmt.Errorf("failed to parse first_seen: %w", err)
		}

		if event.LastSeen, err = time.Parse(time.RFC3339Nano, lastSeen); err != nil {
			return nil, fmt.Errorf("failed to parse last_seen: %w", err)
		}

		events = append(events, event)
	}

	return events, rows.Err()
}

// prune removes the workload's events beyond the most recent limit.
func prune(ctx context.Context, tx *sql.Tx, workloadID string, limit int) error {
	const q = `
		DELETE FROM workload_event
		WHERE workload_id = ?
		AND id NOT IN (
			SELECT id
			FROM workload_event
			WHERE workload_id = ?
			ORDER BY last_seen DESC
			LIMIT ?
		)
	`

	if _, err := tx.ExecContext(ctx, q, workloadID, workloadID, limit); err != nil {
		return fmt.Errorf("failed to prune workload events: %w", err)
	}

	return nil
}
