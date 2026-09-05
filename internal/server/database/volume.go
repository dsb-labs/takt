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
	// ErrVolumeExists is returned when a volume already exists with the given name.
	ErrVolumeExists = errors.New("volume already exists")
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

// Insert records a new volume, returning it with the identifier and creation time
// the server assigned. The given volume's identifier and creation time are ignored:
// both are this repository's to hand out.
//
// Returns ErrVolumeExists when a volume already holds the name. Creating one that
// exists is an error rather than a no-op, because a volume holds data: a caller who
// meant a name they had not used yet would otherwise be handed someone else's.
func (r *VolumeRepository) Insert(ctx context.Context, volume Volume) (Volume, error) {
	const q = `INSERT INTO volume (id, name, labels, owner, mode, created_at) VALUES (?, ?, jsonb(?), ?, ?, ?)`

	encoded, err := marshalLabels(volume.Labels)
	if err != nil {
		return Volume{}, err
	}

	volume.ID = xid.New().String()
	volume.CreatedAt = time.Now().UTC()

	_, err = r.db.ExecContext(ctx, q,
		volume.ID, volume.Name, encoded, volume.Owner, volume.Mode, formatTime(volume.CreatedAt))
	switch {
	case IsUniqueError(err):
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeExists, volume.Name)
	case err != nil:
		return Volume{}, fmt.Errorf("failed to insert volume: %w", err)
	}

	return volume, nil
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

// Update replaces the mutable fields of the volume with the given name, reporting
// ErrVolumeNotFound when no such volume exists.
//
// The labels, the owner and the mode. A volume's name identifies it, its
// identifier is what its directory is named for, and its contents are the
// workloads' to write — so these are the whole of what an update of a volume can
// mean.
func (r *VolumeRepository) Update(ctx context.Context, volume Volume) (Volume, error) {
	const q = `UPDATE volume SET labels = jsonb(?), owner = ?, mode = ? WHERE name = ?`

	encoded, err := marshalLabels(volume.Labels)
	if err != nil {
		return Volume{}, err
	}

	tag, err := r.db.ExecContext(ctx, q, encoded, volume.Owner, volume.Mode, volume.Name)
	if err != nil {
		return Volume{}, fmt.Errorf("failed to update volume: %w", err)
	}

	affected, err := tag.RowsAffected()
	if err != nil {
		return Volume{}, fmt.Errorf("failed to update volume: %w", err)
	}

	if affected == 0 {
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, volume.Name)
	}

	return r.Get(ctx, volume.Name)
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
