package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrWorkloadNotFound is returned when no workload exists with the requested name.
	ErrWorkloadNotFound = errors.New("workload not found")
)

type (
	// The Workload type represents the desired state of a workload as persisted
	// by the server. The spec is stored as canonical JSON so that the wire format
	// remains the single description of a workload's shape.
	Workload struct {
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
func (r *WorkloadRepository) Upsert(ctx context.Context, w Workload) (Workload, bool, error) {
	labels, err := marshalLabels(w.Labels)
	if err != nil {
		return Workload{}, false, err
	}

	existing, err := r.Get(ctx, w.Name)
	switch {
	case errors.Is(err, ErrWorkloadNotFound):
		created, err := r.insert(ctx, w, labels)
		if err != nil {
			return Workload{}, false, err
		}

		return created, true, nil
	case err != nil:
		return Workload{}, false, err
	}

	if existing.SpecHash == w.SpecHash {
		return existing, false, nil
	}

	updated, err := r.update(ctx, w, labels, existing)
	if err != nil {
		return Workload{}, false, err
	}

	return updated, false, nil
}

func (r *WorkloadRepository) insert(ctx context.Context, w Workload, labels string) (Workload, error) {
	const q = `
		INSERT INTO workload (name, version, runtime, schedule, spec, spec_hash, labels, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	now := time.Now().UTC()
	timestamp := formatTime(now)

	_, err := r.db.ExecContext(ctx, q, w.Name, 1, w.Runtime, w.Schedule, string(w.Spec), w.SpecHash, labels, timestamp, timestamp)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to insert workload: %w", err)
	}

	w.Version = 1
	w.CreatedAt = now
	w.UpdatedAt = now

	return w, nil
}

func (r *WorkloadRepository) update(ctx context.Context, w Workload, labels string, existing Workload) (Workload, error) {
	const q = `
		UPDATE workload
		SET version = ?, runtime = ?, schedule = ?, spec = ?, spec_hash = ?, labels = ?, updated_at = ?
		WHERE name = ?
	`

	now := time.Now().UTC()
	version := existing.Version + 1

	tag, err := r.db.ExecContext(ctx, q, version, w.Runtime, w.Schedule, string(w.Spec), w.SpecHash, labels, formatTime(now), w.Name)
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

	w.Version = version
	w.CreatedAt = existing.CreatedAt
	w.UpdatedAt = now

	return w, nil
}

// Get returns the workload with the given name, returning ErrWorkloadNotFound
// when no such workload exists.
func (r *WorkloadRepository) Get(ctx context.Context, name string) (Workload, error) {
	const q = `
		SELECT name, version, runtime, schedule, spec, spec_hash, labels, created_at, updated_at
		FROM workload
		WHERE name = ?
	`

	var (
		w                    Workload
		spec, labels         string
		createdAt, updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q, name).Scan(
		&w.Name, &w.Version, &w.Runtime, &w.Schedule, &spec, &w.SpecHash, &labels, &createdAt, &updatedAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Workload{}, ErrWorkloadNotFound
	case err != nil:
		return Workload{}, fmt.Errorf("failed to load workload: %w", err)
	}

	if err = hydrate(&w, spec, labels, createdAt, updatedAt); err != nil {
		return Workload{}, err
	}

	return w, nil
}

// List returns every workload, ordered by name.
func (r *WorkloadRepository) List(ctx context.Context) ([]Workload, error) {
	const q = `
		SELECT name, version, runtime, schedule, spec, spec_hash, labels, created_at, updated_at
		FROM workload
		ORDER BY name ASC
	`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query workloads: %w", err)
	}

	var workloads []Workload
	defer rows.Close()

	for rows.Next() {
		var (
			w                    Workload
			spec, labels         string
			createdAt, updatedAt string
		)

		if err = rows.Scan(&w.Name, &w.Version, &w.Runtime, &w.Schedule, &spec, &w.SpecHash, &labels, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan workload: %w", err)
		}

		if err = hydrate(&w, spec, labels, createdAt, updatedAt); err != nil {
			return nil, err
		}

		workloads = append(workloads, w)
	}

	return workloads, rows.Err()
}

// Delete removes the workload with the given name, returning ErrWorkloadNotFound
// when no such workload exists.
func (r *WorkloadRepository) Delete(ctx context.Context, name string) error {
	const q = `DELETE FROM workload WHERE name = ?`

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

func hydrate(w *Workload, spec, labels, createdAt, updatedAt string) error {
	parsedLabels, err := unmarshalLabels(labels)
	if err != nil {
		return err
	}

	created, updated, err := parseTimestamps(createdAt, updatedAt)
	if err != nil {
		return err
	}

	w.Spec = []byte(spec)
	w.Labels = parsedLabels
	w.CreatedAt = created
	w.UpdatedAt = updated

	return nil
}
