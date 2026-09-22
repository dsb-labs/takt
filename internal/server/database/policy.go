package database

import (
	"bytes"
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
		// How many times the policy has been written. This is the entity tag a
		// conditional apply compares against, and it moves only when an apply
		// changed the document. Before any apply there is no row, which a
		// caller reads as version zero.
		Version int
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
	return getPolicy(ctx, r.db)
}

// Apply replaces the policy with the given document, on the condition that
// the stored version is still ifMatch. Zero names the state before any
// apply, where no row exists, so a first apply is conditioned the same way
// every later one is.
//
// Reporting ErrPolicyChanged on a mismatch is what turns two concurrent
// applies into one winner and one refusal, rather than a silent overwrite.
//
// A document identical to the stored one is not written, so re-applying the
// same file leaves the version where it is and the tag a pipeline holds stays
// good. Returns the policy as stored, which is what a get would now report.
func (r *PolicyRepository) Apply(ctx context.Context, document []byte, ifMatch int) (Policy, error) {
	var stored Policy

	err := transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		existing, err := getPolicy(ctx, tx)
		switch {
		case errors.Is(err, ErrNoPolicy):
			if ifMatch != 0 {
				return ErrPolicyChanged
			}

			stored, err = insertPolicy(ctx, tx, document)

			return err
		case err != nil:
			return err
		case existing.Version != ifMatch:
			return ErrPolicyChanged
		case bytes.Equal(existing.Document, document):
			stored = existing

			return nil
		}

		stored, err = updatePolicy(ctx, tx, document, existing)

		return err
	})
	if err != nil {
		return Policy{}, err
	}

	return stored, nil
}

// getPolicy reads the policy through anything that can run a query, so that
// the conditional write can read under the same lock it goes on to write with.
func getPolicy(ctx context.Context, q querier) (Policy, error) {
	const stmt = `SELECT document, version, updated_at FROM policy WHERE id = 1`

	var (
		policy    Policy
		updatedAt string
	)

	err := q.QueryRowContext(ctx, stmt).Scan(&policy.Document, &policy.Version, &updatedAt)
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

// insertPolicy writes the first policy.
func insertPolicy(ctx context.Context, tx *sql.Tx, document []byte) (Policy, error) {
	const q = `INSERT INTO policy (id, document, version, updated_at) VALUES (1, ?, 1, ?)`

	now := time.Now().UTC()

	if _, err := tx.ExecContext(ctx, q, document, formatTime(now)); err != nil {
		return Policy{}, fmt.Errorf("failed to store policy: %w", err)
	}

	return Policy{Document: document, Version: 1, UpdatedAt: now}, nil
}

// updatePolicy replaces the stored document, moving the version on.
func updatePolicy(ctx context.Context, tx *sql.Tx, document []byte, existing Policy) (Policy, error) {
	const q = `UPDATE policy SET document = ?, version = ?, updated_at = ? WHERE id = 1`

	now := time.Now().UTC()
	version := existing.Version + 1

	if _, err := tx.ExecContext(ctx, q, document, version, formatTime(now)); err != nil {
		return Policy{}, fmt.Errorf("failed to store policy: %w", err)
	}

	return Policy{Document: document, Version: version, UpdatedAt: now}, nil
}
