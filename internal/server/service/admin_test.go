package service_test

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestAdminService_PrepareBackup(t *testing.T) {
	t.Parallel()

	t.Run("holds the database and nothing else", func(t *testing.T) {
		dir := t.TempDir()
		newBackupDatabase(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.key"), []byte("key"), 0o600))

		assert.Equal(t, []string{"state.db"}, archiveNames(t, streamBackup(t, dir, service.BackupOptions{})))
	})

	t.Run("holds the key when asked", func(t *testing.T) {
		dir := t.TempDir()
		newBackupDatabase(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.key"), []byte("key"), 0o600))

		names := archiveNames(t, streamBackup(t, dir, service.BackupOptions{IncludeKey: true}))
		slices.Sort(names)
		assert.Equal(t, []string{"secret.key", "state.db"}, names)
	})

	// The archive is what an operator restores from, so the database inside it has to
	// be a database rather than something shaped like one.
	t.Run("the database in the archive opens", func(t *testing.T) {
		dir := t.TempDir()

		db := newBackupDatabase(t, dir)
		_, _, err := database.NewWorkloadRepository(db).Upsert(t.Context(), database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash",
		})
		require.NoError(t, err)

		archive := streamBackup(t, dir, service.BackupOptions{})

		restored := filepath.Join(t.TempDir(), "state.db")
		require.NoError(t, os.WriteFile(restored, archiveEntry(t, archive, "state.db"), 0o600))

		opened, err := database.Open(t.Context(), database.Config{
			Logger: newTestLogger(t),
			Path:   restored,
		})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, opened.Close()) })

		stored, err := database.NewWorkloadRepository(opened).List(t.Context())
		require.NoError(t, err)
		require.Len(t, stored, 1)
		assert.Equal(t, "example", stored[0].Name)
	})

	// The backup is assembled beside the database, so a run that finished has to
	// leave the data directory as it found it.
	t.Run("leaves nothing behind", func(t *testing.T) {
		dir := t.TempDir()
		newBackupDatabase(t, dir)

		before := entryNames(t, dir)

		backup, err := newAdminService(t, dir).PrepareBackup(t.Context(), service.BackupOptions{})
		require.NoError(t, err)
		require.NoError(t, backup.Stream(io.Discard))
		require.NoError(t, backup.Close())

		assert.Equal(t, before, entryNames(t, dir))
	})

	// Preparing is separate from streaming so that these two are reported to a caller
	// that can still act on them, rather than as an archive that stops early.
	t.Run("refuses before writing when the database is not there", func(t *testing.T) {
		dir := t.TempDir()

		backup, err := newAdminService(t, dir).PrepareBackup(t.Context(), service.BackupOptions{})
		assert.Nil(t, backup)
		assert.ErrorIs(t, err, database.ErrNoDatabase)

		// The temporary directory a failed preparation made goes with it, or a server
		// asked for a backup it cannot take accumulates one per attempt.
		assert.Empty(t, entryNames(t, dir))
	})

	t.Run("refuses before writing when the key is not there", func(t *testing.T) {
		dir := t.TempDir()
		newBackupDatabase(t, dir)

		before := entryNames(t, dir)

		backup, err := newAdminService(t, dir).PrepareBackup(t.Context(), service.BackupOptions{IncludeKey: true})
		assert.Nil(t, backup)
		assert.Error(t, err)
		assert.Equal(t, before, entryNames(t, dir))
	})
}

// streamBackup prepares a backup of the data directory at dir, streams it, and
// returns the archive.
func streamBackup(t *testing.T, dir string, options service.BackupOptions) []byte {
	t.Helper()

	backup, err := newAdminService(t, dir).PrepareBackup(t.Context(), options)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backup.Close()) })

	var buf bytes.Buffer
	require.NoError(t, backup.Stream(&buf))

	return buf.Bytes()
}

func newAdminService(t *testing.T, dir string) *service.AdminService {
	t.Helper()

	return service.NewAdminService(service.AdminServiceConfig{
		Logger:   newTestLogger(t),
		Database: filepath.Join(dir, "state.db"),
		KeyPath:  filepath.Join(dir, "secret.key"),
	})
}

// newBackupDatabase opens a database in dir so that there is something to back up,
// and returns it for a test that wants to put rows in it.
func newBackupDatabase(t *testing.T, dir string) *sql.DB {
	t.Helper()

	db, err := database.Open(t.Context(), database.Config{
		Logger: newTestLogger(t),
		Path:   filepath.Join(dir, "state.db"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	return db
}

func archiveNames(t *testing.T, archive []byte) []string {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)

	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
	}

	return names
}

func archiveEntry(t *testing.T, archive []byte, name string) []byte {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)

	f, err := reader.Open(name)
	require.NoError(t, err)
	defer f.Close()

	contents, err := io.ReadAll(f)
	require.NoError(t, err)

	return contents
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	slices.Sort(names)

	return names
}
