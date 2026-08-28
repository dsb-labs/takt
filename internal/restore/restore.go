// Package restore puts a node back from a backup archive.
//
// A backup is taken through the API, because a consistent snapshot needs a running
// server. A restore is the opposite: it runs with the server stopped, over the data
// directory directly, and has the configuration file and nothing else to go on.
package restore

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // Registers the driver the database is read back through.
)

var (
	// ErrDatabaseExists is returned when the data directory already holds a
	// database.
	ErrDatabaseExists = errors.New("database already exists")
	// ErrKeyExists is returned when the keyring already holds a key the archive
	// carries.
	ErrKeyExists = errors.New("encryption key already exists")
	// ErrNoDatabase is returned when the archive carries no database.
	ErrNoDatabase = errors.New("archive holds no database")
	// ErrUnknownEntry is returned when the archive carries something a restore
	// does not know where to put.
	ErrUnknownEntry = errors.New("archive holds an unrecognised entry")
)

// The directory a backup archive holds the keyring under, mirroring where the
// keyring sits in a data directory.
const keyringDir = "keys"

// The extension every key file in a keyring carries.
const keyExtension = ".key"

type (
	// The Config type contains fields used to perform a restore.
	//
	// The paths are given rather than derived, because where a node keeps its state
	// is the configuration's answer and not this package's.
	Config struct {
		// The backup archive to read.
		Archive string
		// The SQLite database file the archive's snapshot is written to.
		Database string
		// The directory the keyring is written into, which a configuration file is
		// allowed to put outside the data directory.
		Keys string
		// The directory holding every volume, one per identifier, which is where a
		// restore looks for the volume data it did not put there.
		Volumes string
	}

	// The Report type describes what a restore did, and what it could not do.
	Report struct {
		// The files written, in the order they were written.
		Restored []string
		// The identifiers of keys secrets are sealed under that the keyring does
		// not hold.
		//
		// Every workload reading one of these fails to start, with nothing at the
		// workload pointing at the cause. An archive taken without --include-keys
		// is the usual reason, and the keyring's own backup is the answer.
		MissingKeys []string
		// The volumes the database knows about whose data is not on disk.
		//
		// Reported rather than refused. Volume data is not in the archive, so
		// copying it after the database is a reasonable order to work in, and a
		// restore that failed here would be failing on a step it was never asked
		// to perform.
		MissingVolumes []Volume
	}

	// The Volume type names a volume the database holds a row for.
	Volume struct {
		// The identifier the volume was assigned, which is the name of the
		// directory its data belongs in.
		ID string
		// What an operator calls it.
		Name string
		// Where its data is expected on this host.
		Path string
	}

	// The file type is one entry of an archive, paired with where it is restored
	// to.
	file struct {
		entry *zip.File
		path  string
	}
)

// Run restores a node from a backup archive.
//
// The archive is read in full and checked against where each entry would go before
// anything is written, so an archive holding something unexpected is refused while
// the data directory is still as it was found.
//
// What the restore cannot do is reported rather than performed. Volume data is not
// in a backup, and a keyring may have one of its own, so the two ways a restored
// node fails silently later are named here instead.
func Run(ctx context.Context, config Config) (Report, error) {
	archive, err := zip.OpenReader(config.Archive)
	if err != nil {
		return Report{}, fmt.Errorf("failed to open backup archive: %w", err)
	}
	defer archive.Close()

	files, err := plan(config, archive)
	if err != nil {
		return Report{}, err
	}

	if err = clear(config.Database); err != nil {
		return Report{}, err
	}

	var report Report

	for _, f := range files {
		if err = write(f); err != nil {
			return Report{}, err
		}

		report.Restored = append(report.Restored, f.path)
	}

	if err = inspect(ctx, config, &report); err != nil {
		return Report{}, err
	}

	return report, nil
}

// plan works out where every entry of the archive is restored to, and refuses the
// whole archive rather than part of it.
//
// Nothing is written until this has run. An archive is a file off a shelf, and an
// entry naming a path of its own choosing is the way that file becomes a way to
// write anywhere the restoring user can reach.
func plan(config Config, archive *zip.ReadCloser) ([]file, error) {
	database := filepath.Base(config.Database)

	var files []file
	var restored bool

	for _, entry := range archive.File {
		switch {
		case entry.Name == database:
			files = append(files, file{entry: entry, path: config.Database})
			restored = true
		case strings.HasPrefix(entry.Name, keyringDir+"/"):
			name := strings.TrimPrefix(entry.Name, keyringDir+"/")

			// The remainder has to be a bare file name. Checking for ".." would
			// answer the same question in the archive's terms rather than in the
			// filesystem's, and this refuses a separator of any kind.
			if name == "" || strings.ContainsRune(name, os.PathSeparator) ||
				strings.ContainsRune(name, '/') || !strings.HasSuffix(name, keyExtension) {
				return nil, fmt.Errorf("%w: %s", ErrUnknownEntry, entry.Name)
			}

			files = append(files, file{entry: entry, path: filepath.Join(config.Keys, name)})
		default:
			return nil, fmt.Errorf("%w: %s", ErrUnknownEntry, entry.Name)
		}
	}

	if !restored {
		return nil, fmt.Errorf("%w: expected %s", ErrNoDatabase, database)
	}

	// Checked before the write-ahead log is removed rather than as the first file is
	// written, so a refused restore leaves a data directory that is still whole.
	for _, f := range files {
		if err := vacant(f.path, config.Database); err != nil {
			return nil, err
		}
	}

	return files, nil
}

// vacant reports whether the path is free for the restore to write.
func vacant(path, database string) error {
	_, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("failed to read %s: %w", path, err)
	}

	// A database is refused because replacing one is not something to do without
	// being asked, and moving it aside first is a deliberate step on an action that
	// cannot be undone.
	if path == database {
		return fmt.Errorf("%w: %s, move it aside to restore over it", ErrDatabaseExists, path)
	}

	// A key is refused because an identifier names one key. Writing over a file
	// already answering to it would leave whatever was sealed under the first key
	// unopenable, which is the same reason the keyring itself writes exclusively.
	return fmt.Errorf("%w: %s", ErrKeyExists, path)
}

// clear removes the write-ahead log and shared-memory index left beside a database
// that is no longer there.
//
// This is the step that goes wrong when the procedure is followed by hand. SQLite
// opens a restored database with a stale log beside it perfectly happily, replays
// what the log holds, and the result is a node whose state is quietly not the one
// that was backed up.
func clear(database string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(database + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove %s: %w", database+suffix, err)
		}
	}

	return nil
}

// write one entry of the archive to where the plan put it.
func write(f file) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(f.path), err)
	}

	src, err := f.entry.Open()
	if err != nil {
		return fmt.Errorf("failed to read %s from the backup archive: %w", f.entry.Name, err)
	}
	defer src.Close()

	// Exclusive, because the plan has already established that nothing is there.
	// Between the two is a window this closes rather than reasons about.
	//
	// Readable only by the owner: the database holds every workload's specification,
	// environment included, and the server refuses to start on a key that is readable
	// by anyone else.
	dst, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", f.path, err)
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to write %s: %w", f.path, err)
	}

	// On disk before the restore reports having written it, so a crash after this
	// leaves a file that can be read rather than a name with nothing behind it.
	if err = dst.Sync(); err != nil {
		return fmt.Errorf("failed to flush %s: %w", f.path, err)
	}

	return nil
}
