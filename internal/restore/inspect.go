package restore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// inspect reads the restored database and reports what it refers to that is not on
// this host.
//
// A restore that only unpacked an archive would succeed on a node that goes on to
// run nothing. Both of the things missing here surface much later, as workloads that
// never start, and neither says anything at the workload about why. Naming them now
// is the whole reason for this being a command rather than a paragraph.
func inspect(ctx context.Context, config Config, report *Report) error {
	// Read-only, and deliberately not through the server's own Open, which migrates.
	// A restore reports on the archive as it was taken; migrating it here would make
	// the answer depend on which binary happened to run the restore.
	db, err := sql.Open("sqlite", config.Database+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return fmt.Errorf("failed to open the restored database: %w", err)
	}
	defer db.Close()

	if report.MissingKeys, err = missingKeys(ctx, db, config.Keys); err != nil {
		return err
	}

	if report.MissingVolumes, err = missingVolumes(ctx, db, config.Volumes); err != nil {
		return err
	}

	return nil
}

// missingKeys returns the identifier of every key the secrets are sealed under that
// the keyring does not hold.
//
// The database records which key opens what, so this is the one question that
// decides whether a restored node can read its own secrets. An archive taken without
// --include-keys answers it with every key it names, which is the point: the keyring
// has a backup of its own, and finding that out now beats finding it out from a
// workload that will not start.
func missingKeys(ctx context.Context, db *sql.DB, directory string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT key_id FROM secret ORDER BY key_id")
	if err != nil {
		return nil, fmt.Errorf("failed to read the keys secrets are sealed under: %w", err)
	}
	defer rows.Close()

	var missing []string

	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to read the keys secrets are sealed under: %w", err)
		}

		present, err := exists(filepath.Join(directory, id+keyExtension))
		if err != nil {
			return nil, err
		}

		if !present {
			missing = append(missing, id)
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the keys secrets are sealed under: %w", err)
	}

	return missing, nil
}

// missingVolumes returns every volume the database holds a row for whose data is not
// on this host.
//
// This is the step of a restore that nothing can perform for the operator. Volume
// data is arbitrary and is not in the archive, so the rows arrive without it, and a
// volume is found by the identifier it was assigned rather than by its name —
// recreating one by name gives a fresh identifier, an empty volume, and a row
// pointing at nothing. Naming both the identifier and where its data belongs is what
// makes that recoverable.
func missingVolumes(ctx context.Context, db *sql.DB, directory string) ([]Volume, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name FROM volume ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("failed to read the volumes: %w", err)
	}
	defer rows.Close()

	var missing []Volume

	for rows.Next() {
		var volume Volume
		if err = rows.Scan(&volume.ID, &volume.Name); err != nil {
			return nil, fmt.Errorf("failed to read the volumes: %w", err)
		}

		volume.Path = filepath.Join(directory, volume.ID)

		present, err := exists(volume.Path)
		if err != nil {
			return nil, err
		}

		if !present {
			missing = append(missing, volume)
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the volumes: %w", err)
	}

	return missing, nil
}

// exists reports whether there is anything at the path.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}
}
