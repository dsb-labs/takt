package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

var (
	// ErrNoSnapshotSource is returned when there is no database at the path to
	// snapshot.
	ErrNoSnapshotSource = errors.New("database to snapshot does not exist")
	// ErrSnapshotExists is returned when something is already at the destination.
	ErrSnapshotExists = errors.New("snapshot already exists")
)

// Snapshot writes a consistent copy of the database at source to destination.
//
// A running server keeps its committed state spread across the database, its
// write-ahead log and its shared-memory index, so copying the database file alone
// produces one that is stale or torn. It fails quietly: SQLite opens the result
// happily and the missing transactions are noticed later, if at all. VACUUM INTO is
// one statement that produces a single consistent, compacted file from a live
// connection.
//
// The destination must not exist. SQLite refuses to overwrite one, and left to
// surface on its own that reads as an unexplained failure on the second run rather
// than as the safety property it is.
func Snapshot(ctx context.Context, source, destination string) error {
	_, err := os.Stat(source)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %s", ErrNoSnapshotSource, source)
	case err != nil:
		return fmt.Errorf("failed to read database: %w", err)
	}

	_, err = os.Stat(destination)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s", ErrSnapshotExists, destination)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("failed to read snapshot destination: %w", err)
	}

	// Read-only, and deliberately not through Open, which migrates. A snapshot must
	// not alter the database it is a snapshot of — an older binary taking a backup of
	// a newer database would otherwise migrate it down on the way past. Read-only
	// also means this adds no writer to a database another process is writing to:
	// VACUUM INTO writes the destination and nothing else.
	//
	// The busy timeout still matters. VACUUM INTO holds a read lock for as long as it
	// takes to copy, and it has to acquire one first.
	db, err := sql.Open("sqlite", source+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	if _, err = db.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return fmt.Errorf("failed to snapshot database: %w", err)
	}

	// SQLite creates the file world-readable regardless of what the directory allows,
	// exactly as it does for the database itself. The snapshot holds every workload's
	// specification, environment included, so it is tightened to match what it copied.
	if err = os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("failed to restrict snapshot permissions: %w", err)
	}

	return nil
}
