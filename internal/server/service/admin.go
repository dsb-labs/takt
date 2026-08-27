package service

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dsb-labs/orca/internal/server/database"
)

type (
	// The AdminService type performs the operations an operator runs against the
	// node itself rather than against the workloads on it.
	AdminService struct {
		logger   *slog.Logger
		database string
		keyPath  string
	}

	// The AdminServiceConfig type contains fields used to construct an
	// AdminService.
	AdminServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The SQLite database file a backup snapshots. A backup is prepared beside
		// it, which is the data directory by construction.
		Database string
		// The file holding the key a secret's value is encrypted under.
		KeyPath string
	}

	// The BackupOptions type describes what a backup covers.
	BackupOptions struct {
		// Include the encryption key alongside the database.
		//
		// Off unless asked for. The database and the key are separable on purpose:
		// a database without its key decrypts nothing, which is what makes a copy
		// of it safe to keep somewhere a key would not be. An archive holding both
		// is key material, and every decision about where it is stored has to
		// change to match.
		IncludeKey bool
	}

	// The Backup type is a snapshot that has been taken and is waiting to be read.
	//
	// Preparing and streaming are separate so that a caller learns of a failure
	// before it starts writing a response. Everything that can realistically fail —
	// an absent database, a full disk, a key that is not where it was configured —
	// fails while there is still something to say about it. Once the first byte of
	// an archive has gone out there is no way to retract it, and a truncated zip is
	// a backup that looks like it worked.
	//
	// Close must be called. The snapshot is a file the size of the database.
	Backup struct {
		logger *slog.Logger
		dir    string
		files  []backupFile
	}

	// The backupFile type is one entry of a backup archive: where it is read from,
	// and the name it is written under.
	backupFile struct {
		name string
		path string
	}
)

// NewAdminService returns a new instance of the AdminService type.
func NewAdminService(config AdminServiceConfig) *AdminService {
	return &AdminService{
		logger:   config.Logger.With("component", "admin"),
		database: config.Database,
		keyPath:  config.KeyPath,
	}
}

// PrepareBackup takes a snapshot of what orca holds on disk.
//
// The backup covers a consistent snapshot of the database, and the encryption key
// when the options ask for it. It covers neither volume data, which is arbitrary
// user data orca has no business copying, nor the mounted secret files, which are
// transient and rewritten as a workload starts.
//
// The returned Backup must be closed.
func (s *AdminService) PrepareBackup(ctx context.Context, options BackupOptions) (*Backup, error) {
	if options.IncludeKey {
		// The one backup that carries durable key material. Extracting a secret
		// through the API needs the API to be reachable at the time. The key opens
		// every backup taken before this one and every one taken until it is
		// rotated, so it deserves a record of having happened.
		s.logger.Warn("preparing a backup that includes the secret encryption key")
	}

	// Beside the database rather than in the system temporary directory. That is the
	// one place orca knows it can write and knows is readable only by the user
	// running the server, and a snapshot is the size of the database — which is more
	// than a small tmpfs is willing to hold.
	//
	// A directory rather than a file, because VACUUM INTO refuses to write a file
	// that exists and every way of reserving a temporary file creates it first.
	dir, err := os.MkdirTemp(filepath.Dir(s.database), "backup-")
	if err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	backup := &Backup{logger: s.logger, dir: dir}

	// Named for what they are on disk, so that restoring the archive is a copy
	// rather than a rename somebody has to be told about.
	name := filepath.Base(s.database)

	snapshot := filepath.Join(dir, name)
	if err = database.Snapshot(ctx, s.database, snapshot); err != nil {
		backup.closeAfterFailure()

		return nil, err
	}

	backup.files = append(backup.files, backupFile{name: name, path: snapshot})

	if options.IncludeKey {
		// Read here rather than at the first byte of the archive, so that a key
		// which is not where it was configured is reported as a failed request
		// rather than as an archive missing the thing it was asked for.
		if _, err = os.Stat(s.keyPath); err != nil {
			backup.closeAfterFailure()

			return nil, fmt.Errorf("failed to read encryption key: %w", err)
		}

		backup.files = append(backup.files, backupFile{name: filepath.Base(s.keyPath), path: s.keyPath})
	}

	return backup, nil
}

// Stream the backup to w as a zip archive.
func (b *Backup) Stream(w io.Writer) error {
	archive := zip.NewWriter(w)

	for _, file := range b.files {
		if err := addToArchive(archive, file.path, file.name); err != nil {
			return err
		}
	}

	if err := archive.Close(); err != nil {
		return fmt.Errorf("failed to write backup archive: %w", err)
	}

	return nil
}

// Close removes the snapshot the backup was taken into.
func (b *Backup) Close() error {
	if err := os.RemoveAll(b.dir); err != nil {
		return fmt.Errorf("failed to remove backup directory: %w", err)
	}

	return nil
}

// closeAfterFailure removes the snapshot on a path that has no caller to return the
// error to, because it is already returning a more interesting one.
func (b *Backup) closeAfterFailure() {
	if err := b.Close(); err != nil {
		b.logger.With("error", err, "path", b.dir).Warn("failed to remove backup directory")
	}
}

func addToArchive(archive *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", name, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", name, err)
	}

	// Deflated rather than stored. A SQLite page file compresses well, and the
	// archive goes over a network to somewhere it sits for a long time.
	//
	// Dated, because a zip entry with no time in it extracts to a file dated 1980.
	// Somebody restoring a node checks that they have the right archive, and a date
	// that says nothing is one less way to check.
	entry, err := archive.CreateHeader(&zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: info.ModTime(),
	})
	if err != nil {
		return fmt.Errorf("failed to add %s to the backup archive: %w", name, err)
	}

	if _, err = io.Copy(entry, f); err != nil {
		return fmt.Errorf("failed to write %s to the backup archive: %w", name, err)
	}

	return nil
}
