package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrVolumeNotFound is returned when no volume exists with the requested name.
	ErrVolumeNotFound = errors.New("volume not found")
)

type (
	// The Volume type represents storage that outlives the workloads mounting it.
	//
	// A volume is desired state like a workload, but there is nothing running to
	// observe for one: it is a directory, and it either exists or it does not. What
	// makes it its own resource rather than a field on a workload is its lifetime.
	// Deleting the workload that mounts a volume leaves the volume, so removing
	// stored data is always something asked for.
	Volume struct {
		// The identifier the server assigns to the volume. The directory holding the
		// volume's data is named for this rather than for the name, which is the
		// operator's handle and is never used as a path component.
		ID string
		// The name that identifies the volume.
		Name string
		// Arbitrary key-value pairs attached to the volume.
		Labels map[string]string
		// Who owns the volume's directory, as a numeric "uid" or "uid:gid".
		// Empty leaves the directory owned by the user running the server.
		Owner string
		// The permission bits on the volume's directory, as an octal string.
		// Empty leaves the directory readable only by the user running the
		// server.
		Mode string
		// The time the volume was created.
		CreatedAt time.Time
	}

	// The VolumeRepository type provides persistence operations for the volume
	// domain.
	VolumeRepository struct {
		db *sql.DB
	}
)

// NewVolumeRepository returns a VolumeRepository backed by the given database.
func NewVolumeRepository(db *sql.DB) *VolumeRepository {
	return &VolumeRepository{db: db}
}

// Upsert stores the volume, creating it when no volume holds the name and
// replacing the labels, the owner and the mode when one does. The second
// return reports whether it was created. The given volume's identifier and
// creation time are ignored: both are this repository's to hand out.
//
// An upsert rather than an insert-or-fail, because applying the same manifest
// twice means the file is the truth, which is how a workload's apply already
// behaves. The name identifies the volume, the directory holding its data is
// named for the identifier it keeps across an update, and its contents are
// the workloads' to write — so these fields are the whole of what an apply
// can change.
func (r *VolumeRepository) Upsert(ctx context.Context, volume Volume) (Volume, bool, error) {
	const q = `
		INSERT INTO volume (id, name, labels, owner, mode, created_at)
		VALUES (?, ?, jsonb(?), ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			labels = excluded.labels,
			owner = excluded.owner,
			mode = excluded.mode
		RETURNING id, created_at
	`

	encoded, err := marshalLabels(volume.Labels)
	if err != nil {
		return Volume{}, false, err
	}

	// The identifier is generated ahead of the write. A conflict keeps the
	// existing row's identifier, so the returned one reports which happened.
	id := xid.New().String()

	var createdAt string

	err = r.db.QueryRowContext(ctx, q,
		id, volume.Name, encoded, volume.Owner, volume.Mode, formatTime(time.Now().UTC())).
		Scan(&volume.ID, &createdAt)
	if err != nil {
		return Volume{}, false, fmt.Errorf("failed to upsert volume: %w", err)
	}

	volume.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Volume{}, false, fmt.Errorf("failed to parse created_at: %w", err)
	}

	return volume, volume.ID == id, nil
}

// Get returns the volume with the given name, reporting ErrVolumeNotFound when no
// such volume exists.
func (r *VolumeRepository) Get(ctx context.Context, name string) (Volume, error) {
	const q = `SELECT id, name, json(labels), owner, mode, created_at FROM volume WHERE name = ?`

	var (
		volume            Volume
		labels, createdAt string
	)

	err := r.db.QueryRowContext(ctx, q, name).
		Scan(&volume.ID, &volume.Name, &labels, &volume.Owner, &volume.Mode, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	case err != nil:
		return Volume{}, fmt.Errorf("failed to query volume: %w", err)
	}

	if volume.Labels, err = unmarshalLabels(labels); err != nil {
		return Volume{}, err
	}

	volume.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Volume{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	return volume, nil
}

// List returns the volumes matching every one of the given queries, ordered by
// name so that the result is stable. Passing no queries returns every volume.
//
// A query's path reaches the volume's labels under $.labels, the same way a
// workload query does. Filtering happens in the database rather than in the
// caller, so a query that matches little doesn't cost a read of everything it
// discards. Returns ErrInvalidQueryPath when a query names a path SQLite cannot
// parse.
func (r *VolumeRepository) List(ctx context.Context, queries ...Query) ([]Volume, error) {
	const q = `
		SELECT id, name, json(labels), owner, mode, created_at
		FROM volume
	`

	if err := validPaths(ctx, r.db, queries); err != nil {
		return nil, err
	}

	where, args := filter(labelSource, queries)

	rows, err := r.db.QueryContext(ctx, q+where+"\n\t\tORDER BY name ASC\n\t", args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query volumes: %w", err)
	}

	var volumes []Volume
	defer rows.Close()

	for rows.Next() {
		var (
			volume            Volume
			labels, createdAt string
		)

		if err = rows.Scan(&volume.ID, &volume.Name, &labels, &volume.Owner, &volume.Mode, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan volume: %w", err)
		}

		if volume.Labels, err = unmarshalLabels(labels); err != nil {
			return nil, err
		}

		volume.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse created_at: %w", err)
		}

		volumes = append(volumes, volume)
	}

	return volumes, rows.Err()
}

// Delete removes the volume with the given name, reporting ErrVolumeNotFound when no
// such volume exists.
//
// Only the row. The directory holding the data is the service's to remove, so that
// nothing here touches the filesystem.
func (r *VolumeRepository) Delete(ctx context.Context, name string) error {
	const q = `DELETE FROM volume WHERE name = ?`

	result, err := r.db.ExecContext(ctx, q, name)
	if err != nil {
		return fmt.Errorf("failed to delete volume: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to count deleted volumes: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	}

	return nil
}

// UsedBy returns the names of the workloads whose specifications mount the volume
// with the given name.
//
// The search runs in the database rather than by reading every workload and
// inspecting it here. A volume's mounts are an array in the stored specification, so
// json_each expands them and the comparison happens where the rows are.
//
// A workload being deleted still counts. Its containers are still running until the
// reconciler has torn them down, so a volume it mounts is still in use.
func (r *VolumeRepository) UsedBy(ctx context.Context, name string) ([]string, error) {
	const q = `
		SELECT DISTINCT w.name
		FROM workload AS w, json_each(json_extract(w.spec, '$.volumes')) AS mount
		WHERE json_extract(mount.value, '$.name') = ?
		ORDER BY w.name ASC
	`

	rows, err := r.db.QueryContext(ctx, q, name)
	if err != nil {
		return nil, fmt.Errorf("failed to query volume users: %w", err)
	}

	var workloads []string
	defer rows.Close()

	for rows.Next() {
		var workload string
		if err = rows.Scan(&workload); err != nil {
			return nil, fmt.Errorf("failed to scan volume user: %w", err)
		}

		workloads = append(workloads, workload)
	}

	return workloads, rows.Err()
}
