package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrWorkloadNotFound is returned when no workload exists with the requested name.
	ErrWorkloadNotFound = errors.New("workload not found")
	// ErrInvalidQueryPath is returned when a query names a path SQLite cannot parse.
	ErrInvalidQueryPath = errors.New("invalid query path")
)

type (
	// The Workload type represents the desired state of a workload as persisted
	// by the server. The spec is stored as canonical JSON so that the wire format
	// remains the single description of a workload's shape.
	Workload struct {
		// The identifier the server assigns to the workload. It exists so that rows
		// referring to a workload do not depend on its name, which is the operator's
		// handle and could otherwise never change; it is not exposed by the API.
		ID string
		// The name that identifies the workload.
		Name string
		// Incremented every time the workload's specification changes.
		Version int
		// Which runtime block the specification names, and so which driver runs it.
		Runtime string
		// The cron expression describing when the workload should run, or empty
		// when it should run continuously.
		Schedule string
		// The canonical JSON encoding of the workload's specification.
		Spec []byte
		// The hash of Spec, used to detect drift between desired and running state.
		SpecHash string
		// Arbitrary key-value pairs attached to the workload.
		Labels map[string]string
		// The time the workload was first applied.
		CreatedAt time.Time
		// The time the workload's specification last changed.
		UpdatedAt time.Time
		// The time the workload was marked for deletion, or the zero time when it
		// has not been. A workload being deleted keeps its row until the driver
		// reports its work is gone, so that the teardown is observable and the
		// reconciler is the only thing that removes running work.
		DeletedAt time.Time
	}

	// The Query type matches workloads whose stored specification has the given
	// value at the given path.
	//
	// The path is a SQLite JSON path over the whole specification, so a query can
	// reach anything the specification holds — including labels, which live under
	// $.labels.
	Query struct {
		// The JSON path into the specification, such as "$.labels.app".
		Path string
		// The value the path must hold, compared as text.
		Value string
	}

	// The WorkloadRepository type provides persistence operations for the workload domain.
	WorkloadRepository struct {
		db *sql.DB
	}
)

// NewWorkloadRepository returns a WorkloadRepository backed by the given database.
func NewWorkloadRepository(db *sql.DB) *WorkloadRepository {
	return &WorkloadRepository{db: db}
}

// Upsert stores w as the desired state for its name, returning the stored
// workload and whether it was newly created.
//
// The write is conditional on the specification actually having changed: when the
// incoming SpecHash matches what is already stored, the row is left untouched and
// the existing workload is returned, so re-applying an unchanged manifest neither
// bumps the version nor moves UpdatedAt. When the hash differs the version is
// incremented, which is what causes the reconciler to replace running instances.
//
// The Version, CreatedAt and UpdatedAt fields of w are ignored; the repository
// assigns them.
func (r *WorkloadRepository) Upsert(ctx context.Context, w Workload, ports ...Port) (Workload, bool, error) {
	labels, err := marshalLabels(w.Labels)
	if err != nil {
		return Workload{}, false, err
	}

	var stored Workload
	var created bool

	// The row and its port allocations are written together. A workload whose ports
	// could not be claimed must not exist at all: the reconciler would otherwise
	// start it against a specification naming host ports nothing holds, so the
	// caller would be told the apply failed while orca ran it anyway.
	err = transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		existing, err := get(ctx, tx, w.Name)
		switch {
		case errors.Is(err, ErrWorkloadNotFound):
			if stored, err = insert(ctx, tx, w, labels); err != nil {
				return err
			}

			created = true
		case err != nil:
			return err
		case existing.SpecHash == w.SpecHash:
			// Nothing about the specification changed, so the row is left alone. The
			// ports are still reclaimed below, since an allocation may have been
			// released and re-resolved to the same values.
			stored = existing
		default:
			if stored, err = update(ctx, tx, w, labels, existing); err != nil {
				return err
			}
		}

		for i := range ports {
			ports[i].WorkloadID = stored.ID
		}

		return claim(ctx, tx, stored.ID, ports)
	})
	if err != nil {
		return Workload{}, false, err
	}

	return stored, created, nil
}

func insert(ctx context.Context, tx *sql.Tx, w Workload, labels string) (Workload, error) {
	const q = `
		INSERT INTO workload (id, name, version, runtime, schedule, spec, spec_hash, labels, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, jsonb(?), ?, jsonb(?), ?, ?)
	`

	now := time.Now().UTC()
	timestamp := formatTime(now)

	id := xid.New().String()

	_, err := tx.ExecContext(ctx, q, id, w.Name, 1, w.Runtime, w.Schedule, string(w.Spec), w.SpecHash, labels, timestamp, timestamp)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to insert workload: %w", err)
	}

	w.ID = id
	w.Version = 1
	w.CreatedAt = now
	w.UpdatedAt = now

	return w, nil
}

func update(ctx context.Context, tx *sql.Tx, w Workload, labels string, existing Workload) (Workload, error) {
	const q = `
		UPDATE workload
		SET version = ?, runtime = ?, schedule = ?, spec = jsonb(?), spec_hash = ?, labels = jsonb(?), updated_at = ?
		WHERE name = ?
	`

	now := time.Now().UTC()
	version := existing.Version + 1

	tag, err := tx.ExecContext(ctx, q, version, w.Runtime, w.Schedule, string(w.Spec), w.SpecHash, labels, formatTime(now), w.Name)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to update workload: %w", err)
	}

	affected, err := tag.RowsAffected()
	switch {
	case err != nil:
		return Workload{}, fmt.Errorf("failed to count updated workloads: %w", err)
	case affected == 0:
		return Workload{}, ErrWorkloadNotFound
	}

	w.ID = existing.ID
	w.Version = version
	w.CreatedAt = existing.CreatedAt
	w.UpdatedAt = now

	return w, nil
}

// Get returns the workload with the given name, returning ErrWorkloadNotFound
// when no such workload exists.
func (r *WorkloadRepository) Get(ctx context.Context, name string) (Workload, error) {
	return get(ctx, r.db, name)
}

// The querier interface lets a read run either on the pool or inside a transaction,
// so Upsert can check for an existing workload without leaving the transaction it is
// about to write in.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func get(ctx context.Context, q querier, name string) (Workload, error) {
	const query = `
		SELECT id, name, version, runtime, schedule, json(spec), spec_hash, json(labels), created_at, updated_at, deleted_at
		FROM workload
		WHERE name = ?
	`

	var (
		w                               Workload
		spec, labels                    string
		createdAt, updatedAt, deletedAt string
	)

	err := q.QueryRowContext(ctx, query, name).Scan(
		&w.ID, &w.Name, &w.Version, &w.Runtime, &w.Schedule, &spec, &w.SpecHash, &labels, &createdAt, &updatedAt, &deletedAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to load workload: %w", err)
	}

	if err = hydrate(&w, spec, labels, createdAt, updatedAt, deletedAt); err != nil {
		return Workload{}, err
	}

	return w, nil
}

// List returns the workloads matching every one of the given queries, ordered by
// name. Passing no queries returns every workload.
//
// Filtering happens in the database rather than in the caller, so a query that
// matches little doesn't cost a read of everything it discards. Returns
// ErrInvalidQueryPath when a query names a path SQLite cannot parse.
func (r *WorkloadRepository) List(ctx context.Context, queries ...Query) ([]Workload, error) {
	const q = `
		SELECT id, name, version, runtime, schedule, json(spec), spec_hash, json(labels), created_at, updated_at, deleted_at
		FROM workload
	`

	if err := r.validPaths(ctx, queries); err != nil {
		return nil, err
	}

	where, args := filter(queries)

	rows, err := r.db.QueryContext(ctx, q+where+"\n\t\tORDER BY name ASC\n\t", args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query workloads: %w", err)
	}

	var workloads []Workload
	defer rows.Close()

	for rows.Next() {
		var (
			w                               Workload
			spec, labels                    string
			createdAt, updatedAt, deletedAt string
		)

		if err = rows.Scan(&w.ID, &w.Name, &w.Version, &w.Runtime, &w.Schedule, &spec, &w.SpecHash, &labels, &createdAt, &updatedAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("failed to scan workload: %w", err)
		}

		if err = hydrate(&w, spec, labels, createdAt, updatedAt, deletedAt); err != nil {
			return nil, err
		}

		workloads = append(workloads, w)
	}

	return workloads, rows.Err()
}

// MarkDeleting records that the workload with the given name should be deleted,
// returning the marked workload.
//
// The row is deliberately kept: the reconciler is the only thing that stops
// running work, so the desired state has to survive long enough for it to notice
// and act. Marking is idempotent — a workload already marked keeps its original
// timestamp — so a repeated request neither fails nor restarts the clock. Returns
// ErrWorkloadNotFound when no such workload exists.
func (r *WorkloadRepository) MarkDeleting(ctx context.Context, name string) (Workload, error) {
	const q = `
		UPDATE workload
		SET deleted_at = ?
		WHERE name = ? AND deleted_at = ''
	`

	existing, err := r.Get(ctx, name)
	if err != nil {
		return Workload{}, err
	}

	if !existing.DeletedAt.IsZero() {
		return existing, nil
	}

	now := time.Now().UTC()

	if _, err = r.db.ExecContext(ctx, q, formatTime(now), name); err != nil {
		return Workload{}, fmt.Errorf("failed to mark workload for deletion: %w", err)
	}

	existing.DeletedAt = now

	return existing, nil
}

// Delete removes the workload with the given name, returning ErrWorkloadNotFound
// when no such workload exists.
//
// This is the final removal of desired state, and is the reconciler's to call once
// the driver reports the workload's work is gone. Callers wanting to delete a
// workload should use MarkDeleting.
func (r *WorkloadRepository) Delete(ctx context.Context, name string) error {
	const q = `DELETE FROM workload WHERE name = ?`

	// The workload's port allocations go with it by ON DELETE CASCADE, which is why
	// the database is opened with foreign keys enforced.
	tag, err := r.db.ExecContext(ctx, q, name)
	if err != nil {
		return fmt.Errorf("failed to delete workload: %w", err)
	}

	affected, err := tag.RowsAffected()
	switch {
	case err != nil:
		return fmt.Errorf("failed to count deleted workloads: %w", err)
	case affected == 0:
		return ErrWorkloadNotFound
	}

	return nil
}

// filter builds the WHERE clause matching every query, along with its arguments.
//
// Each comparison is made as text so that a caller which only has strings — a CLI, a
// URL query parameter — matches a number in the specification as readily as a string.
// The path is bound as a parameter rather than interpolated, so a query cannot reach
// beyond the value it is inspecting.
func filter(queries []Query) (string, []any) {
	if len(queries) == 0 {
		return "", nil
	}

	clauses := make([]string, 0, len(queries))
	args := make([]any, 0, len(queries)*2)

	for _, query := range queries {
		clauses = append(clauses, "CAST(json_extract(spec, ?) AS TEXT) = ?")
		args = append(args, query.Path, query.Value)
	}

	return "\n\t\tWHERE " + strings.Join(clauses, "\n\t\t  AND "), args
}

// validPaths reports whether SQLite can parse every query's path.
//
// A malformed path fails the whole query, which would otherwise surface as an
// internal error well after the caller could have been told they mistyped something.
// The check asks SQLite to parse each path against an empty object: it needs no data,
// and agreeing with the engine that will run the query is the point — a hand-written
// approximation would accept paths the query then rejects.
func (r *WorkloadRepository) validPaths(ctx context.Context, queries []Query) error {
	const q = `SELECT json_extract('{}', ?)`

	for _, query := range queries {
		var ignored sql.NullString
		if err := r.db.QueryRowContext(ctx, q, query.Path).Scan(&ignored); err != nil {
			return fmt.Errorf("%w: %q", ErrInvalidQueryPath, query.Path)
		}
	}

	return nil
}

func hydrate(w *Workload, spec, labels, createdAt, updatedAt, deletedAt string) error {
	parsedLabels, err := unmarshalLabels(labels)
	if err != nil {
		return err
	}

	created, updated, err := parseTimestamps(createdAt, updatedAt)
	if err != nil {
		return err
	}

	deleted, err := parseOptionalTime(deletedAt)
	if err != nil {
		return fmt.Errorf("failed to parse deleted_at: %w", err)
	}

	w.Spec = []byte(spec)
	w.Labels = parsedLabels
	w.CreatedAt = created
	w.UpdatedAt = updated
	w.DeletedAt = deleted

	return nil
}
