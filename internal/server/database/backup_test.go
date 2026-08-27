package database

import (
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshot(t *testing.T) {
	t.Parallel()

	t.Run("copies what was committed", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "state.db")

		db := openForTest(t, source)

		repo := NewWorkloadRepository(db)
		_, _, err := repo.Upsert(t.Context(), Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash",
		})
		require.NoError(t, err)

		destination := filepath.Join(t.TempDir(), "backup.db")
		require.NoError(t, Snapshot(t.Context(), source, destination))

		// Opened as an ordinary database rather than inspected as a file. A snapshot
		// nothing can open is not a backup, and that is the failure this whole
		// operation exists to avoid.
		restored := openForTest(t, destination)

		stored, err := NewWorkloadRepository(restored).List(t.Context())
		require.NoError(t, err)
		require.Len(t, stored, 1)
		assert.Equal(t, "example", stored[0].Name)
	})

	// The reason the command exists. A server is writing while the backup is taken,
	// so the snapshot has to hold what was committed before it started and nothing
	// that was still in flight.
	t.Run("excludes an uncommitted write", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "state.db")

		db := openForTest(t, source)

		repo := NewWorkloadRepository(db)
		_, _, err := repo.Upsert(t.Context(), Workload{
			Name:     "committed",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash",
		})
		require.NoError(t, err)

		// Left open for the duration of the snapshot, so the change below is written
		// and not committed while VACUUM INTO reads.
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(t.Context(), `UPDATE workload SET spec_hash = ?`, "in-flight")
		require.NoError(t, err)

		destination := filepath.Join(t.TempDir(), "backup.db")
		require.NoError(t, Snapshot(t.Context(), source, destination))

		restored := openForTest(t, destination)

		stored, err := NewWorkloadRepository(restored).List(t.Context())
		require.NoError(t, err)
		require.Len(t, stored, 1)
		assert.Equal(t, "committed", stored[0].Name)
		assert.Equal(t, "hash", stored[0].SpecHash)
	})

	// SQLite creates the file world-readable whatever the directory allows, and the
	// snapshot holds every workload's specification.
	t.Run("restricts permissions", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "state.db")
		openForTest(t, source)

		destination := filepath.Join(t.TempDir(), "backup.db")
		require.NoError(t, Snapshot(t.Context(), source, destination))

		info, err := os.Stat(destination)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o077, "snapshot is readable beyond its owner: %v", info.Mode().Perm())
	})

	// A backup of nothing that reports success is the worst outcome available: it is
	// discovered when the database it was meant to replace has already gone.
	t.Run("refuses a database that is not there", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "state.db")
		destination := filepath.Join(t.TempDir(), "backup.db")

		assert.ErrorIs(t, Snapshot(t.Context(), source, destination), ErrNoSnapshotSource)
		assert.NoFileExists(t, destination)
	})

	t.Run("refuses a destination that exists", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "state.db")
		openForTest(t, source)

		destination := filepath.Join(t.TempDir(), "backup.db")
		require.NoError(t, os.WriteFile(destination, []byte("existing"), 0o600))

		assert.ErrorIs(t, Snapshot(t.Context(), source, destination), ErrSnapshotExists)

		// Refused rather than emptied. The file that was there is somebody's, and a
		// refusal that destroyed it first would be no refusal at all.
		contents, err := os.ReadFile(destination)
		require.NoError(t, err)
		assert.Equal(t, "existing", string(contents))
	})

	// The snapshot must not migrate what it reads. An older binary backing up a newer
	// database would otherwise carry it down a version on the way past.
	t.Run("leaves the source untouched", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "state.db")
		openForTest(t, source)

		before, err := os.ReadFile(source)
		require.NoError(t, err)

		require.NoError(t, Snapshot(t.Context(), source, filepath.Join(t.TempDir(), "backup.db")))

		after, err := os.ReadFile(source)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})
}

func openForTest(t *testing.T, path string) *sql.DB {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	db, err := Open(t.Context(), Config{
		Logger: slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: level})),
		Path:   path,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	return db
}
