package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNoCurrentKey is returned when the database records no current encryption
	// key.
	ErrNoCurrentKey = errors.New("no current encryption key")
)

type (
	// The EncryptionKey type records a key a secret's value is sealed under.
	//
	// The key itself is not here. The database records which key sealed what, and
	// the keyring on disk holds the bytes, which is what keeps a copy of the
	// database useless to anyone without the keys.
	EncryptionKey struct {
		// The identifier naming the key in the keyring.
		ID string
		// Whether new secrets are sealed under this key. Exactly one key is
		// current, enforced by the schema.
		IsCurrent bool
		// The time the key was recorded.
		CreatedAt time.Time
	}

	// The EncryptionKeyRepository type provides persistence operations for the
	// encryption key domain.
	EncryptionKeyRepository struct {
		db *sql.DB
	}
)

// NewEncryptionKeyRepository returns an EncryptionKeyRepository backed by the given
// database.
func NewEncryptionKeyRepository(db *sql.DB) *EncryptionKeyRepository {
	return &EncryptionKeyRepository{db: db}
}

// Current returns the key new secrets are sealed under, reporting ErrNoCurrentKey
// when the database records none.
func (r *EncryptionKeyRepository) Current(ctx context.Context) (EncryptionKey, error) {
	const q = `SELECT id, is_current, created_at FROM encryption_key WHERE is_current = 1`

	var (
		key       EncryptionKey
		createdAt string
	)

	err := r.db.QueryRowContext(ctx, q).Scan(&key.ID, &key.IsCurrent, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return EncryptionKey{}, ErrNoCurrentKey
	case err != nil:
		return EncryptionKey{}, fmt.Errorf("failed to query the current encryption key: %w", err)
	}

	if key.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return EncryptionKey{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	return key, nil
}

// List returns every key the database records, newest first.
func (r *EncryptionKeyRepository) List(ctx context.Context) ([]EncryptionKey, error) {
	const q = `SELECT id, is_current, created_at FROM encryption_key ORDER BY created_at DESC`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query encryption keys: %w", err)
	}
	defer rows.Close()

	var keys []EncryptionKey

	for rows.Next() {
		var (
			key       EncryptionKey
			createdAt string
		)

		if err = rows.Scan(&key.ID, &key.IsCurrent, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan encryption key: %w", err)
		}

		if key.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, fmt.Errorf("failed to parse created_at: %w", err)
		}

		keys = append(keys, key)
	}

	return keys, rows.Err()
}

// Adopt records the given key as the current one, which is how a server with an
// empty database takes ownership of the key it just generated.
//
// Refused when a current key is already recorded. Two servers starting against one
// database would otherwise each generate a key and seal secrets under different
// ones.
func (r *EncryptionKeyRepository) Adopt(ctx context.Context, id string) error {
	const q = `INSERT INTO encryption_key (id, is_current, created_at) VALUES (?, 1, ?)`

	if _, err := r.db.ExecContext(ctx, q, id, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("failed to record the current encryption key: %w", err)
	}

	return nil
}
