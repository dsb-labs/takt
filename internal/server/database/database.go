// Package database provides the SQLite-backed persistence layer for the orca server.
//
// The database stores desired state only — what the operator asked for. What is
// actually running is observed from the driver on demand, so nothing here can go
// stale against reality.
package database

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"modernc.org/sqlite"
)

const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

//go:embed migrations/*.sql
var migrations embed.FS

// The Config type contains fields used to open the database.
type Config struct {
	// The logger used for database lifecycle events.
	Logger *slog.Logger
	// The filesystem path to the SQLite database file.
	Path string
}

// Open opens (or creates) the SQLite database at the path in config and runs any
// pending migrations. The returned pool must be closed by the caller when no
// longer needed.
func Open(ctx context.Context, config Config) (*sql.DB, error) {
	logger := config.Logger.With("component", "database")

	// Each of these pragmas is here for a specific reason, and each has to travel in
	// the DSN rather than be executed once after opening: database/sql pools
	// connections, and a one-shot PRAGMA applies only to whichever connection
	// happened to run it.
	//
	// foreign_keys is off by default in SQLite, and orca relies on it: a workload's
	// port allocations are removed by ON DELETE CASCADE rather than by hand, so
	// without enforcement a delete would silently leak host ports that nothing owns
	// and nothing reclaims.
	//
	// busy_timeout makes a connection wait for a contended write lock instead of
	// immediately failing with SQLITE_BUSY. Without it, concurrent applies fail
	// outright — an operator applying a directory of manifests in parallel loses
	// most of them — because SQLite permits one writer at a time and orca has
	// several: the API accepting applies and the reconciler finishing deletions.
	//
	// journal_mode=wal lets readers run while a write is in progress, which matters
	// because reads are not rare here: every list and get reads the workload table
	// while the reconciler may be writing to it.
	db, err := sql.Open("sqlite", config.Path+
		"?_pragma=foreign_keys(1)"+
		"&_pragma=busy_timeout(5000)"+
		"&_pragma=journal_mode(wal)")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err = migrateUp(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	logger.With("path", config.Path).Debug("database opened")

	return db, nil
}

// IsUniqueError reports whether err is a unique-key or primary-key constraint violation.
func IsUniqueError(err error) bool {
	sqliteErr, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return false
	}

	return sqliteErr.Code() == sqliteConstraintPrimaryKey || sqliteErr.Code() == sqliteConstraintUnique
}

// transaction runs fn inside a transaction, committing when it returns nil and
// rolling back otherwise. A rollback that itself fails is joined onto the original
// error so neither is lost.
func transaction(ctx context.Context, db *sql.DB, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	if err = fn(ctx, tx); err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			return err
		}

		return errors.Join(err, rollbackErr)
	}

	if err = tx.Commit(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

func migrateUp(db *sql.DB) error {
	m, err := newMigrator(db)
	if err != nil {
		return err
	}

	if err = m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to apply migrations: %w", err)
	}

	return nil
}

func newMigrator(db *sql.DB) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("failed to load migration source: %w", err)
	}

	driver, err := sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to construct migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		return nil, fmt.Errorf("failed to construct migrator: %w", err)
	}

	return m, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func marshalLabels(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "{}", nil
	}

	data, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("failed to encode labels: %w", err)
	}

	return string(data), nil
}

func unmarshalLabels(data string) (map[string]string, error) {
	if data == "" || data == "{}" {
		return nil, nil
	}

	var labels map[string]string
	if err := json.Unmarshal([]byte(data), &labels); err != nil {
		return nil, fmt.Errorf("failed to decode labels: %w", err)
	}

	return labels, nil
}

// parseOptionalTime parses a timestamp column that is empty when unset, which is
// how SQLite carries a nullable time without a NULL.
func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	return time.Parse(time.RFC3339Nano, value)
}

func parseTimestamps(createdAt, updatedAt string) (time.Time, time.Time, error) {
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return created, updated, nil
}
