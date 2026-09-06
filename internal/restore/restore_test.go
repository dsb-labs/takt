package restore_test

import (
	"archive/zip"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/restore"
	"github.com/dsb-labs/takt/internal/server/database"
)

// The identifiers used throughout, which are xid values because that is what takt
// assigns and what the volume directories are named after.
const (
	testKeyID    = "cvhs0dq0kqj4c9r8m1a0"
	testVolumeID = "cvhs0dq0kqj4c9r8m1ag"
)

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("puts the database and the keyring back", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir, withKeys(testKeyID))

		report, err := restore.Run(t.Context(), configWith(config, archive))
		require.NoError(t, err)

		assert.FileExists(t, config.Database)
		assert.FileExists(t, filepath.Join(config.Keys, testKeyID+".key"))
		assert.Equal(t, []string{
			config.Database,
			filepath.Join(config.Keys, testKeyID+".key"),
		}, report.Restored)

		// Readable only by the owner. The database holds every workload's
		// specification, environment included, and the server refuses to start on a
		// key that is readable by anyone else.
		for _, path := range report.Restored {
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s is readable by more than its owner", path)
		}
	})

	t.Run("removes a write-ahead log left beside the database", func(t *testing.T) {
		t.Parallel()

		// The step that goes wrong when the procedure is followed by hand. SQLite
		// replays a stale log against a restored database perfectly happily, and the
		// node comes up holding state that is quietly not what was backed up.
		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir, withKeys(testKeyID))

		require.NoError(t, os.MkdirAll(filepath.Dir(config.Database), 0o700))

		stale := []string{config.Database + "-wal", config.Database + "-shm"}
		for _, path := range stale {
			require.NoError(t, os.WriteFile(path, []byte("stale"), 0o600))
		}

		_, err := restore.Run(t.Context(), configWith(config, archive))
		require.NoError(t, err)

		for _, path := range stale {
			assert.NoFileExists(t, path, "a stale %s survived the restore", filepath.Ext(path))
		}
	})

	t.Run("reports the keys the keyring does not hold", func(t *testing.T) {
		t.Parallel()

		// An archive taken without --include-keys, which is the default. Every
		// workload reading a secret fails to start without these, and nothing at the
		// workload says why.
		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir)

		report, err := restore.Run(t.Context(), configWith(config, archive))
		require.NoError(t, err)

		assert.Equal(t, []string{testKeyID}, report.MissingKeys)
	})

	t.Run("reports the volumes whose data is not on the host", func(t *testing.T) {
		t.Parallel()

		// The step nothing can perform for the operator. Volume data is not in the
		// archive, so the row arrives without it, and the identifier is what names
		// the directory it belongs in.
		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir)

		report, err := restore.Run(t.Context(), configWith(config, archive))
		require.NoError(t, err)

		assert.Equal(t, []restore.Volume{{
			ID:   testVolumeID,
			Name: "example-data",
			Path: filepath.Join(config.Volumes, testVolumeID),
		}}, report.MissingVolumes)
	})

	t.Run("says nothing is missing when everything is there", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir, withKeys(testKeyID))

		require.NoError(t, os.MkdirAll(filepath.Join(config.Volumes, testVolumeID), 0o700))

		report, err := restore.Run(t.Context(), configWith(config, archive))
		require.NoError(t, err)

		assert.Empty(t, report.MissingKeys)
		assert.Empty(t, report.MissingVolumes)
	})

	t.Run("refuses to replace a database that is already there", func(t *testing.T) {
		t.Parallel()

		// Replacing a live database is not something to do without being asked, and
		// moving it aside first is a deliberate step on an action nothing undoes.
		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir)

		require.NoError(t, os.MkdirAll(filepath.Dir(config.Database), 0o700))
		require.NoError(t, os.WriteFile(config.Database, []byte("in use"), 0o600))

		_, err := restore.Run(t.Context(), configWith(config, archive))
		assert.ErrorIs(t, err, restore.ErrDatabaseExists)
	})

	t.Run("refuses to replace a key the keyring already holds", func(t *testing.T) {
		t.Parallel()

		// An identifier names one key. Writing over the file answering to it would
		// leave whatever the first key sealed unopenable.
		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir, withKeys(testKeyID))

		require.NoError(t, os.MkdirAll(config.Keys, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(config.Keys, testKeyID+".key"), []byte("other"), 0o600))

		_, err := restore.Run(t.Context(), configWith(config, archive))
		assert.ErrorIs(t, err, restore.ErrKeyExists)
	})

	t.Run("leaves the data directory alone when it refuses", func(t *testing.T) {
		t.Parallel()

		// Refused before anything is written rather than part way through, so what a
		// failed restore leaves behind is what it found.
		dir := t.TempDir()
		config := configFor(dir)
		archive := backup(t, dir, withKeys(testKeyID))

		require.NoError(t, os.MkdirAll(config.Keys, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(config.Keys, testKeyID+".key"), []byte("other"), 0o600))
		require.NoError(t, os.WriteFile(config.Database+"-wal", []byte("stale"), 0o600))

		_, err := restore.Run(t.Context(), configWith(config, archive))
		require.Error(t, err)

		assert.NoFileExists(t, config.Database, "a refused restore wrote the database anyway")
		assert.FileExists(t, config.Database+"-wal", "a refused restore removed the write-ahead log")
	})

	t.Run("refuses an archive holding something it cannot place", func(t *testing.T) {
		t.Parallel()

		// An archive is a file off a shelf. An entry naming a path of its own
		// choosing is how that file becomes a way to write wherever the restoring
		// user can reach.
		names := []string{
			"../escape",
			"keys/../../escape.key",
			"keys/nested/key.key",
			"exec/state/example.json",
			"volumes/" + testVolumeID + "/data",
		}

		for _, name := range names {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				config := configFor(dir)
				archive := backup(t, dir, withEntry(name))

				_, err := restore.Run(t.Context(), configWith(config, archive))
				assert.ErrorIs(t, err, restore.ErrUnknownEntry)
			})
		}
	})

	t.Run("refuses an archive that is not a backup", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		config := configFor(dir)

		path := filepath.Join(dir, "empty.zip")
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, zip.NewWriter(f).Close())
		require.NoError(t, f.Close())

		_, err = restore.Run(t.Context(), configWith(config, path))
		assert.ErrorIs(t, err, restore.ErrNoDatabase)
	})
}

// configFor returns the paths a restore writes to, laid out as a data directory is.
func configFor(dir string) restore.Config {
	data := filepath.Join(dir, "restored")

	return restore.Config{
		Database: filepath.Join(data, "state.db"),
		Keys:     filepath.Join(data, "keys"),
		Volumes:  filepath.Join(data, "volumes"),
	}
}

// configWith returns the config pointed at an archive.
func configWith(config restore.Config, archive string) restore.Config {
	config.Archive = archive

	return config
}

// The option type describes what a test's archive holds beyond the database.
type option func(*zip.Writer, *testing.T)

// withKeys puts key files in the archive, as --include-keys does.
func withKeys(ids ...string) option {
	return func(archive *zip.Writer, t *testing.T) {
		for _, id := range ids {
			entry, err := archive.Create("keys/" + id + ".key")
			require.NoError(t, err)

			_, err = entry.Write(make([]byte, 32))
			require.NoError(t, err)
		}
	}
}

// withEntry puts an arbitrary entry in the archive, standing in for one a restore
// has no business unpacking.
func withEntry(name string) option {
	return func(archive *zip.Writer, t *testing.T) {
		_, err := archive.Create(name)
		require.NoError(t, err)
	}
}

// backup writes an archive shaped like the one the backup endpoint streams, over a
// database holding a secret and a volume.
//
// A real database rather than a fixture, because what a restore reports comes out of
// the schema. One written by hand would answer whatever the test asked it to.
func backup(t *testing.T, dir string, options ...option) string {
	t.Helper()

	source := filepath.Join(dir, "source", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o700))

	db, err := database.Open(context.Background(), database.Config{
		Logger: slog.New(slog.DiscardHandler),
		Path:   source,
	})
	require.NoError(t, err)

	seed(t, db)
	require.NoError(t, db.Close())

	path := filepath.Join(dir, "backup.zip")

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	archive := zip.NewWriter(f)

	entry, err := archive.Create("state.db")
	require.NoError(t, err)

	contents, err := os.ReadFile(source)
	require.NoError(t, err)

	_, err = entry.Write(contents)
	require.NoError(t, err)

	for _, option := range options {
		option(archive, t)
	}

	require.NoError(t, archive.Close())

	return path
}

// seed writes the rows a restore reports on: a secret, which names the key that has
// to be in the keyring, and a volume, which names the directory that has to be on
// the host.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.Exec(
		`INSERT INTO encryption_key (id, is_current, created_at) VALUES (?, 1, datetime('now'))`,
		testKeyID)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO secret (id, name, value, revision, key_id, created_at, updated_at)
		 VALUES (?, 'example', ?, 'rev', ?, datetime('now'), datetime('now'))`,
		"cvhs0dq0kqj4c9r8m1b0", []byte("sealed"), testKeyID)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO volume (id, name, created_at) VALUES (?, 'example-data', datetime('now'))`,
		testVolumeID)
	require.NoError(t, err)
}
