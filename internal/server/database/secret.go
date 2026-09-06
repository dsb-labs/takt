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
	// ErrSecretsChanged is returned when the secrets a rekey was asked to rewrite
	// are not the secrets the database holds.
	ErrSecretsChanged = errors.New("secrets changed during the rekey")
)

type (
	// The Secret type represents a value a workload can read but an operator cannot,
	// stored encrypted.
	//
	// The value never leaves the database in the clear except to be handed to a
	// workload that is starting. Nothing reports it back to a caller, which is what
	// makes the encrypted column the only copy takt keeps.
	Secret struct {
		// The identifier the server assigns to the secret.
		ID string
		// The name that identifies the secret, and which a manifest references.
		Name string
		// The encrypted value. Empty on a secret that was read without it, since
		// most reads have no business decrypting one.
		Value []byte
		// The identifier of the key the value is sealed under.
		KeyID string
		// Changes whenever the value changes, and never otherwise.
		//
		// This is what a referencing workload's specification hash is mixed with, so
		// that rotating a secret replaces the instances reading it. The value itself
		// is not part of that hash: a hash is reported by the API, and one computed
		// over a value would confirm a guess at it.
		Revision string
		// Arbitrary key-value pairs attached to the secret.
		//
		// Readable back, where the value deliberately is not. A label on a secret is
		// as public as the secret's name.
		Labels map[string]string
		// The time the secret was created.
		CreatedAt time.Time
		// The time the secret last changed, by its value or its labels. The revision
		// is what says the value moved.
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
func (r *SecretRepository) Upsert(
	ctx context.Context, name string, value []byte, revision, keyID string, labels map[string]string,
) (Secret, error) {
	const q = `
		INSERT INTO secret (id, name, value, revision, key_id, labels, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, jsonb(?), ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			value = excluded.value,
			revision = excluded.revision,
			key_id = excluded.key_id,
			labels = excluded.labels,
			updated_at = excluded.updated_at
		RETURNING id, created_at, updated_at
	`

	timestamp := formatTime(time.Now().UTC())

	encoded, err := marshalLabels(labels)
	if err != nil {
		return Secret{}, err
	}

	secret := Secret{
		ID:       xid.New().String(),
		Name:     name,
		Value:    value,
		Revision: revision,
		KeyID:    keyID,
		Labels:   labels,
	}

	var (
		createdAt string
		updatedAt string
	)

	err = r.db.QueryRowContext(ctx, q, secret.ID, name, value, revision, keyID, encoded, timestamp, timestamp).
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
	const q = `SELECT id, name, value, revision, key_id, json(labels), created_at, updated_at FROM secret WHERE name = ?`

	var (
		secret    Secret
		labels    string
		createdAt string
		updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q, name).
		Scan(&secret.ID, &secret.Name, &secret.Value, &secret.Revision, &secret.KeyID, &labels, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Secret{}, fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	case err != nil:
		return Secret{}, fmt.Errorf("failed to query secret: %w", err)
	}

	if secret.Labels, err = unmarshalLabels(labels); err != nil {
		return Secret{}, err
	}

	if secret.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Secret{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if secret.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Secret{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return secret, nil
}

// List returns the secrets matching every one of the given queries, ordered by
// name so that the result is stable. Passing no queries returns every secret.
//
// A query's path reaches the secret's labels under $.labels, the same way a
// workload query does. The filter never sees a value. A query can reach only
// the labels, so it cannot probe what a secret holds. Returns
// ErrInvalidQueryPath when a query names a path SQLite cannot parse.
//
// The values are left behind. Listing secrets is how an operator finds out what
// exists, which needs no decryption, and a read that does not carry a value cannot
// leak one.
func (r *SecretRepository) List(ctx context.Context, queries ...Query) ([]Secret, error) {
	const q = `
		SELECT id, name, revision, json(labels), created_at, updated_at
		FROM secret
	`

	if err := validPaths(ctx, r.db, queries); err != nil {
		return nil, err
	}

	where, args := filter(labelSource, queries)

	rows, err := r.db.QueryContext(ctx, q+where+"\n\t\tORDER BY name ASC\n\t", args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query secrets: %w", err)
	}

	var secrets []Secret
	defer rows.Close()

	for rows.Next() {
		var (
			secret    Secret
			labels    string
			createdAt string
			updatedAt string
		)

		if err = rows.Scan(&secret.ID, &secret.Name, &secret.Revision, &labels, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan secret: %w", err)
		}

		if secret.Labels, err = unmarshalLabels(labels); err != nil {
			return nil, err
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

// ListSealed returns every secret including its value and the key that sealed it,
// ordered by name.
//
// Unlike List, this carries the ciphertext. Only a rekey has business reading every
// sealed value at once, which is why the ordinary listing leaves it out.
func (r *SecretRepository) ListSealed(ctx context.Context) ([]Secret, error) {
	const q = `SELECT id, name, value, revision, key_id, created_at, updated_at FROM secret ORDER BY name ASC`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query secrets: %w", err)
	}
	defer rows.Close()

	var secrets []Secret

	for rows.Next() {
		var (
			secret    Secret
			createdAt string
			updatedAt string
		)

		err = rows.Scan(&secret.ID, &secret.Name, &secret.Value, &secret.Revision, &secret.KeyID, &createdAt, &updatedAt)
		if err != nil {
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

// Rekey replaces every secret's sealed value with the one given for it and records
// keyID as the key they are now sealed under, in one transaction.
//
// This is what makes a rekey safe to interrupt. The re-sealed values and the pointer
// to the current key move together, so a process that dies leaves either every
// secret under the old key or every secret under the new one. There is no state
// where the database and the keyring disagree about which key opens what, and so
// nothing to reconcile afterwards.
//
// The sealed values are keyed by secret name. A name with no entry is left alone,
// which cannot happen when the caller passes back what ListSealed gave it, and would
// mean a partial rekey if it did — so it is refused rather than skipped.
//
// The revision does not move. A rekey changes how a value is stored, not what it is,
// so nothing referencing a secret is redeployed by one.
func (r *SecretRepository) Rekey(ctx context.Context, keyID string, sealed map[string][]byte) error {
	return transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		const (
			countQ   = `SELECT count(*) FROM secret`
			updateQ  = `UPDATE secret SET value = ?, key_id = ? WHERE name = ?`
			demoteQ  = `UPDATE encryption_key SET is_current = 0 WHERE is_current = 1`
			promoteQ = `INSERT INTO encryption_key (id, is_current, created_at) VALUES (?, 1, ?)`
		)

		// Counted inside the transaction rather than trusted from the caller's read.
		// A secret created between the read and here would keep its old key while
		// everything around it moved, and nothing afterwards would say so.
		var count int
		if err := tx.QueryRowContext(ctx, countQ).Scan(&count); err != nil {
			return fmt.Errorf("failed to count secrets: %w", err)
		}

		if count != len(sealed) {
			return fmt.Errorf("%w: %d secrets to rewrite, %d were resealed", ErrSecretsChanged, count, len(sealed))
		}

		// The key is recorded before anything references it. A secret pointed at a
		// key with no row would violate the foreign key, which is the constraint
		// keeping the database and the keyring in step.
		//
		// Demoted before the new one is promoted, because exactly one key may be
		// current and the schema is what enforces it.
		if _, err := tx.ExecContext(ctx, demoteQ); err != nil {
			return fmt.Errorf("failed to retire the previous encryption key: %w", err)
		}

		if _, err := tx.ExecContext(ctx, promoteQ, keyID, formatTime(time.Now().UTC())); err != nil {
			return fmt.Errorf("failed to record the new encryption key: %w", err)
		}

		for name, value := range sealed {
			result, err := tx.ExecContext(ctx, updateQ, value, keyID, name)
			if err != nil {
				return fmt.Errorf("failed to reseal secret %s: %w", name, err)
			}

			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("failed to reseal secret %s: %w", name, err)
			}

			// The counts matching is not enough on its own: a secret deleted and
			// another created between the read and here leaves the total unchanged.
			if affected == 0 {
				return fmt.Errorf("%w: secret %s is no longer there", ErrSecretsChanged, name)
			}
		}

		return nil
	})
}
