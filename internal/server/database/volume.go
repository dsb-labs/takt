package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrVolumeNotFound is returned when no volume exists with the requested name.
	ErrVolumeNotFound = errors.New("volume not found")
	// ErrVolumeChanged is returned when the volume an apply was conditioned on is
	// not the volume the database holds.
	ErrVolumeChanged = errors.New("volume changed since it was read")
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
		// How many times the volume has been written. This is the entity tag a
		// conditional apply compares against, and it moves only when an apply
		// changed something.
		Version int
		// The time the volume was created.
		CreatedAt time.Time
		// The time the volume was last changed by an apply.
		UpdatedAt time.Time
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
// return reports whether it was created. The given volume's identifier,
// version and timestamps are ignored: all of them are this repository's to
// hand out.
//
// An upsert rather than an insert-or-fail, because applying the same manifest
// twice means the file is the truth, which is how a workload's apply already
// behaves. The name identifies the volume, the directory holding its data is
// named for the identifier it keeps across an update, and its contents are
// the workloads' to write — so these fields are the whole of what an apply
// can change.
//
// A non-zero ifMatch conditions the write on the stored version still being
// that one, reporting ErrVolumeChanged when it is not and ErrVolumeNotFound
// when there is no row to have the version at all. Zero applies
// unconditionally, which is what a caller creating a volume has to do: there
// is no version yet to name.
//
// Like a workload's apply, a write that would change nothing is skipped, so
// re-applying an unchanged manifest leaves the version where it is. That is
// what lets a pipeline apply the same file repeatedly without invalidating
// the tag it is holding.
func (r *VolumeRepository) Upsert(ctx context.Context, volume Volume, ifMatch int) (Volume, bool, error) {
	encoded, err := marshalLabels(volume.Labels)
	if err != nil {
		return Volume{}, false, err
	}

	var (
		stored  Volume
		created bool
	)

	err = transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		existing, err := getVolume(ctx, tx, volume.Name)
		switch {
		case errors.Is(err, ErrVolumeNotFound):
			if ifMatch != 0 {
				return err
			}

			stored, err = insertVolume(ctx, tx, volume, encoded)

			created = true

			return err
		case err != nil:
			return err
		case ifMatch != 0 && existing.Version != ifMatch:
			return ErrVolumeChanged
		case unchangedVolume(existing, volume):
			stored = existing

			return nil
		}

		stored, err = updateVolume(ctx, tx, volume, encoded, existing)

		return err
	})
	if err != nil {
		return Volume{}, false, err
	}

	return stored, created, nil
}

// Get returns the volume with the given name, reporting ErrVolumeNotFound when no
// such volume exists.
func (r *VolumeRepository) Get(ctx context.Context, name string) (Volume, error) {
	return getVolume(ctx, r.db, name)
}

// getVolume reads a volume through anything that can run a query, so that the
// conditional write can read under the same lock it goes on to write with.
func getVolume(ctx context.Context, q querier, name string) (Volume, error) {
	const stmt = `
		SELECT id, name, json(labels), owner, mode, version, created_at, updated_at
		FROM volume
		WHERE name = ?
	`

	var (
		volume                       Volume
		labels, createdAt, updatedAt string
	)

	err := q.QueryRowContext(ctx, stmt, name).Scan(&volume.ID, &volume.Name, &labels,
		&volume.Owner, &volume.Mode, &volume.Version, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Volume{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, name)
	case err != nil:
		return Volume{}, fmt.Errorf("failed to query volume: %w", err)
	}

	if volume.Labels, err = unmarshalLabels(labels); err != nil {
		return Volume{}, err
	}

	if volume.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Volume{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if volume.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Volume{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return volume, nil
}

// insertVolume writes a volume that does not exist yet.
func insertVolume(ctx context.Context, tx *sql.Tx, volume Volume, labels string) (Volume, error) {
	const q = `
		INSERT INTO volume (id, name, labels, owner, mode, version, created_at, updated_at)
		VALUES (?, ?, jsonb(?), ?, ?, 1, ?, ?)
	`

	now := time.Now().UTC()
	timestamp := formatTime(now)

	volume.ID = xid.New().String()

	_, err := tx.ExecContext(ctx, q, volume.ID, volume.Name, labels,
		volume.Owner, volume.Mode, timestamp, timestamp)
	if err != nil {
		return Volume{}, fmt.Errorf("failed to insert volume: %w", err)
	}

	volume.Version = 1
	volume.CreatedAt = now
	volume.UpdatedAt = now

	return volume, nil
}

// updateVolume replaces the fields an apply owns, moving the version on.
func updateVolume(ctx context.Context, tx *sql.Tx, volume Volume, labels string, existing Volume) (Volume, error) {
	const q = `
		UPDATE volume
		SET labels = jsonb(?), owner = ?, mode = ?, version = ?, updated_at = ?
		WHERE name = ?
	`

	now := time.Now().UTC()

	volume.ID = existing.ID
	volume.Version = existing.Version + 1
	volume.CreatedAt = existing.CreatedAt
	volume.UpdatedAt = now

	_, err := tx.ExecContext(ctx, q, labels, volume.Owner, volume.Mode,
		volume.Version, formatTime(now), volume.Name)
	if err != nil {
		return Volume{}, fmt.Errorf("failed to update volume: %w", err)
	}

	return volume, nil
}

// unchangedVolume reports whether an apply would leave the stored volume
// exactly as it is, which is the case where the write is skipped.
func unchangedVolume(existing, incoming Volume) bool {
	return existing.Owner == incoming.Owner &&
		existing.Mode == incoming.Mode &&
		maps.Equal(existing.Labels, incoming.Labels)
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
		SELECT id, name, json(labels), owner, mode, version, created_at, updated_at
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
			volume                       Volume
			labels, createdAt, updatedAt string
		)

		if err = rows.Scan(&volume.ID, &volume.Name, &labels, &volume.Owner,
			&volume.Mode, &volume.Version, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan volume: %w", err)
		}

		if volume.Labels, err = unmarshalLabels(labels); err != nil {
			return nil, err
		}

		if volume.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, fmt.Errorf("failed to parse created_at: %w", err)
		}

		if volume.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, fmt.Errorf("failed to parse updated_at: %w", err)
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
