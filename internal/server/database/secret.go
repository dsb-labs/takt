package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrSecretNotFound is returned when no secret exists with the requested name.
	ErrSecretNotFound = errors.New("secret not found")
)

type (
	// The Secret type represents a value a workload can read but an operator cannot,
	// stored encrypted.
	//
	// The value never leaves the database in the clear except to be handed to a
	// workload that is starting. Nothing reports it back to a caller, which is what
	// makes the encrypted column the only copy orca keeps.
	Secret struct {
		// The identifier the server assigns to the secret.
		ID string
		// The name that identifies the secret, and which a manifest references.
		Name string
		// The encrypted value. Empty on a secret that was read without it, since
		// most reads have no business decrypting one.
		Value []byte
		// Changes whenever the value changes, and never otherwise.
		//
		// This is what a referencing workload's specification hash is mixed with, so
		// that rotating a secret replaces the instances reading it. The value itself
		// is not part of that hash: a hash is reported by the API, and one computed
		// over a value would confirm a guess at it.
		Revision string
		// The time the secret was created.
		CreatedAt time.Time
		// The time the secret's value last changed.
		UpdatedAt time.Time
	}

	// The SecretRepository type provides persistence operations for the secret
	// domain.
	SecretRepository struct {
		db *sql.DB
	}
)

// NewSecretRepository returns a SecretRepository backed by the given database.
func NewSecretRepository(db *sql.DB) *SecretRepository {
	return &SecretRepository{db: db}
}

// Upsert stores value as the secret with the given name, returning the stored
// secret.
//
// The write is unconditional: whether the value actually changed is the caller's to
// decide, because only the caller can decrypt what is already there to compare.
// Calling this at all therefore means the value moved, and the revision passed with
// it is the new one.
//
// The creation time is preserved on a secret that already existed, so rotating one
// does not read as creating it again.
func (r *SecretRepository) Upsert(ctx context.Context, name string, value []byte, revision string) (Secret, error) {
	const q = `
		INSERT INTO secret (id, name, value, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			value = excluded.value,
			revision = excluded.revision,
			updated_at = excluded.updated_at
		RETURNING id, created_at, updated_at
	`

	timestamp := formatTime(time.Now().UTC())

	secret := Secret{
		ID:       xid.New().String(),
		Name:     name,
		Value:    value,
		Revision: revision,
	}

	var (
		createdAt string
		updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q, secret.ID, name, value, revision, timestamp, timestamp).
		Scan(&secret.ID, &createdAt, &updatedAt)
	if err != nil {
		return Secret{}, fmt.Errorf("failed to upsert secret: %w", err)
	}

	if secret.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Secret{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if secret.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Secret{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return secret, nil
}

// Get returns the secret with the given name, including its encrypted value,
// reporting ErrSecretNotFound when no such secret exists.
func (r *SecretRepository) Get(ctx context.Context, name string) (Secret, error) {
	const q = `SELECT id, name, value, revision, created_at, updated_at FROM secret WHERE name = ?`

	var (
		secret    Secret
		createdAt string
		updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q, name).
		Scan(&secret.ID, &secret.Name, &secret.Value, &secret.Revision, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Secret{}, fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	case err != nil:
		return Secret{}, fmt.Errorf("failed to query secret: %w", err)
	}

	if secret.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Secret{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if secret.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Secret{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return secret, nil
}

// List returns every secret, ordered by name so that the result is stable.
//
// The values are left behind. Listing secrets is how an operator finds out what
// exists, which needs no decryption, and a read that does not carry a value cannot
// leak one.
func (r *SecretRepository) List(ctx context.Context) ([]Secret, error) {
	const q = `SELECT id, name, revision, created_at, updated_at FROM secret ORDER BY name ASC`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query secrets: %w", err)
	}

	var secrets []Secret
	defer rows.Close()

	for rows.Next() {
		var (
			secret    Secret
			createdAt string
			updatedAt string
		)

		if err = rows.Scan(&secret.ID, &secret.Name, &secret.Revision, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan secret: %w", err)
		}

		if secret.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, fmt.Errorf("failed to parse created_at: %w", err)
		}

		if secret.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, fmt.Errorf("failed to parse updated_at: %w", err)
		}

		secrets = append(secrets, secret)
	}

	return secrets, rows.Err()
}

// Delete removes the secret with the given name, reporting ErrSecretNotFound when
// no such secret exists.
//
// The links naming it are left behind. A workload that references a deleted secret
// still has to report what it is missing, and re-creating the secret has to move
// that workload's hash again.
func (r *SecretRepository) Delete(ctx context.Context, name string) error {
	const q = `DELETE FROM secret WHERE name = ?`

	result, err := r.db.ExecContext(ctx, q, name)
	if err != nil {
		return fmt.Errorf("failed to delete secret: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to count deleted secrets: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}

	return nil
}

// Revisions returns the current revision of each named secret, keyed by name.
//
// A name that no secret holds is absent from the result rather than an error: the
// caller knows which names it asked about, and deciding what a missing secret means
// is its business. One query rather than one per name, so that hashing a workload
// costs the same however many secrets it reads.
func (r *SecretRepository) Revisions(ctx context.Context, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}

	q := `SELECT name, revision FROM secret WHERE name IN (?` + strings.Repeat(", ?", len(names)-1) + `)`

	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query secret revisions: %w", err)
	}

	revisions := make(map[string]string, len(names))
	defer rows.Close()

	for rows.Next() {
		var name, revision string
		if err = rows.Scan(&name, &revision); err != nil {
			return nil, fmt.Errorf("failed to scan secret revision: %w", err)
		}

		revisions[name] = revision
	}

	return revisions, rows.Err()
}

// UsedBy returns the names of the workloads referencing the secret with the given
// name.
//
// Read from the recorded links rather than by matching the reference text inside
// each stored specification, so the answer does not depend on how a reference is
// written or on where in a manifest one is allowed.
//
// A workload being deleted still counts. Its instances run until the reconciler has
// torn them down, so a secret it reads is still in use.
func (r *SecretRepository) UsedBy(ctx context.Context, name string) ([]string, error) {
	const q = `
		SELECT w.name
		FROM workload_secret AS s
		INNER JOIN workload AS w ON w.id = s.workload_id
		WHERE s.secret_name = ?
		ORDER BY w.name ASC
	`

	rows, err := r.db.QueryContext(ctx, q, name)
	if err != nil {
		return nil, fmt.Errorf("failed to query secret users: %w", err)
	}

	var workloads []string
	defer rows.Close()

	for rows.Next() {
		var workload string
		if err = rows.Scan(&workload); err != nil {
			return nil, fmt.Errorf("failed to scan secret user: %w", err)
		}

		workloads = append(workloads, workload)
	}

	return workloads, rows.Err()
}

// linkSecrets records which secrets a workload references inside an existing
// transaction, so that it can be composed with the write of the workload itself.
//
// Replaced wholesale for the same reason a port allocation is: the stored links have
// to describe the specification that was just written, so a reference removed from a
// manifest stops counting as a use.
func linkSecrets(ctx context.Context, tx *sql.Tx, workloadID string, names []string) error {
	const (
		clear  = `DELETE FROM workload_secret WHERE workload_id = ?`
		insert = `INSERT INTO workload_secret (workload_id, secret_name) VALUES (?, ?)`
	)

	if _, err := tx.ExecContext(ctx, clear, workloadID); err != nil {
		return fmt.Errorf("failed to clear workload secrets: %w", err)
	}

	for _, name := range names {
		if _, err := tx.ExecContext(ctx, insert, workloadID, name); err != nil {
			return fmt.Errorf("failed to record workload secret: %w", err)
		}
	}

	return nil
}
