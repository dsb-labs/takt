package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNoPolicy is returned when no policy document has been applied yet.
	ErrNoPolicy = errors.New("no policy")
	// ErrPolicyChanged is returned when the policy an apply was conditioned on
	// is not the policy the database holds.
	ErrPolicyChanged = errors.New("policy changed since it was read")
)

type (
	// The Policy type represents the applied access-control policy document.
	Policy struct {
		// The canonical form of the document, as the server stores and
		// returns it.
		Document []byte
		// The entity tag a conditional apply compares against. Derived from
		// the document, so a gitops pipeline reading twice sees the same tag.
		ETag string
		// The time the document was last applied.
		UpdatedAt time.Time
	}

	// The PolicyRepository type provides persistence operations for the
	// policy domain.
	PolicyRepository struct {
		db *sql.DB
	}
)

// NewPolicyRepository returns a PolicyRepository backed by the given database.
func NewPolicyRepository(db *sql.DB) *PolicyRepository {
	return &PolicyRepository{db: db}
}

// Get returns the applied policy, reporting ErrNoPolicy when none has been
// applied yet.
func (r *PolicyRepository) Get(ctx context.Context) (Policy, error) {
	const q = `SELECT document, etag, updated_at FROM policy WHERE id = 1`

	var (
		policy    Policy
		updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q).Scan(&policy.Document, &policy.ETag, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Policy{}, ErrNoPolicy
	case err != nil:
		return Policy{}, fmt.Errorf("failed to load policy: %w", err)
	}

	if policy.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Policy{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return policy, nil
}

// Apply replaces the policy with the given document and tag, on the condition
// that the stored tag still equals previousETag. An empty previousETag means
// no policy is expected to exist yet.
//
// Reporting ErrPolicyChanged on a mismatch is what turns two concurrent
// applies into one winner and one refusal, rather than a silent overwrite.
func (r *PolicyRepository) Apply(ctx context.Context, document []byte, etag, previousETag string) error {
	return transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		const read = `SELECT etag FROM policy WHERE id = 1`

		var current string
		err := tx.QueryRowContext(ctx, read).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to load the current policy: %w", err)
		}

		if current != previousETag {
			return ErrPolicyChanged
		}

		const write = `
			INSERT INTO policy (id, document, etag, updated_at)
			VALUES (1, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				document = excluded.document,
				etag = excluded.etag,
				updated_at = excluded.updated_at
		`

		if _, err = tx.ExecContext(ctx, write, document, etag, formatTime(time.Now().UTC())); err != nil {
			return fmt.Errorf("failed to store policy: %w", err)
		}

		return nil
	})
}
