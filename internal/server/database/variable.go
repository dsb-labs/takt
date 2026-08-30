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
	// ErrVariableNotFound is returned when no variable exists with the requested name.
	ErrVariableNotFound = errors.New("variable not found")
)

type (
	// The Variable type represents a value a workload reads, stored in the clear.
	//
	// This is the counterpart to Secret, and the difference is the whole point of
	// having both. A variable is reported by the API with its value, so it is the
	// thing to reach for when an operator wants to see what a workload is configured
	// with. Anything that would be damaging to report belongs in a Secret instead.
	Variable struct {
		// The identifier the server assigns to the variable.
		ID string
		// The name that identifies the variable, and which a manifest references.
		Name string
		// The value the variable holds.
		//
		// Read on every path that reads a variable at all, unlike a secret's, which is
		// left behind by everything but a workload starting.
		Value string
		// Arbitrary key-value pairs attached to the variable.
		Labels map[string]string
		// The time the variable was created.
		CreatedAt time.Time
		// The time the variable last changed, by its value or its labels.
		UpdatedAt time.Time
	}

	// The VariableRepository type provides persistence operations for the variable
	// domain.
	VariableRepository struct {
		db *sql.DB
	}
)

// NewVariableRepository returns a VariableRepository backed by the given database.
func NewVariableRepository(db *sql.DB) *VariableRepository {
	return &VariableRepository{db: db}
}

// Upsert stores value as the variable with the given name, returning the stored
// variable.
//
// The creation time is preserved on a variable that already existed, so changing one
// does not read as creating it again.
func (r *VariableRepository) Upsert(ctx context.Context, name, value string, labels map[string]string) (Variable, error) {
	const q = `
		INSERT INTO variable (id, name, value, labels, created_at, updated_at)
		VALUES (?, ?, ?, jsonb(?), ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			value = excluded.value,
			labels = excluded.labels,
			updated_at = excluded.updated_at
		RETURNING id, created_at, updated_at
	`

	timestamp := formatTime(time.Now().UTC())

	encoded, err := marshalLabels(labels)
	if err != nil {
		return Variable{}, err
	}

	variable := Variable{
		ID:     xid.New().String(),
		Name:   name,
		Value:  value,
		Labels: labels,
	}

	var (
		createdAt string
		updatedAt string
	)

	err = r.db.QueryRowContext(ctx, q, variable.ID, name, value, encoded, timestamp, timestamp).
		Scan(&variable.ID, &createdAt, &updatedAt)
	if err != nil {
		return Variable{}, fmt.Errorf("failed to upsert variable: %w", err)
	}

	if variable.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Variable{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if variable.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Variable{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return variable, nil
}

// Get returns the variable with the given name, reporting ErrVariableNotFound when
// no such variable exists.
func (r *VariableRepository) Get(ctx context.Context, name string) (Variable, error) {
	const q = `SELECT id, name, value, json(labels), created_at, updated_at FROM variable WHERE name = ?`

	var (
		variable  Variable
		labels    string
		createdAt string
		updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q, name).
		Scan(&variable.ID, &variable.Name, &variable.Value, &labels, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Variable{}, fmt.Errorf("%w: %s", ErrVariableNotFound, name)
	case err != nil:
		return Variable{}, fmt.Errorf("failed to query variable: %w", err)
	}

	if variable.Labels, err = unmarshalLabels(labels); err != nil {
		return Variable{}, err
	}

	if variable.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Variable{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if variable.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Variable{}, fmt.Errorf("failed to parse updated_at: %w", err)
	}

	return variable, nil
}

// List returns the variables matching every one of the given queries, ordered by
// name so that the result is stable. Passing no queries returns every variable.
//
// A query's path reaches the variable's labels under $.labels, the same way a
// workload query does. Returns ErrInvalidQueryPath when a query names a path
// SQLite cannot parse.
//
// The values come with them, which is where this differs from the secret
// repository. Listing secrets deliberately leaves the values behind because a read
// that does not carry one cannot leak one. A variable's value is reported by the API
// anyway, so withholding it here would only mean reading each one again.
func (r *VariableRepository) List(ctx context.Context, queries ...Query) ([]Variable, error) {
	const q = `
		SELECT id, name, value, json(labels), created_at, updated_at
		FROM variable
	`

	if err := validPaths(ctx, r.db, queries); err != nil {
		return nil, err
	}

	where, args := filter(labelSource, queries)

	rows, err := r.db.QueryContext(ctx, q+where+"\n\t\tORDER BY name ASC\n\t", args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query variables: %w", err)
	}

	var variables []Variable
	defer rows.Close()

	for rows.Next() {
		var (
			variable  Variable
			labels    string
			createdAt string
			updatedAt string
		)

		if err = rows.Scan(&variable.ID, &variable.Name, &variable.Value, &labels, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan variable: %w", err)
		}

		if variable.Labels, err = unmarshalLabels(labels); err != nil {
			return nil, err
		}

		if variable.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, fmt.Errorf("failed to parse created_at: %w", err)
		}

		if variable.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, fmt.Errorf("failed to parse updated_at: %w", err)
		}

		variables = append(variables, variable)
	}

	return variables, rows.Err()
}

// Delete removes the variable with the given name, reporting ErrVariableNotFound
// when no such variable exists.
//
// The links naming it are left behind, as they are for a secret. A workload that
// references a deleted variable still has to report what it is missing, and
// re-creating the variable has to move that workload's hash again.
func (r *VariableRepository) Delete(ctx context.Context, name string) error {
	const q = `DELETE FROM variable WHERE name = ?`

	result, err := r.db.ExecContext(ctx, q, name)
	if err != nil {
		return fmt.Errorf("failed to delete variable: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to count deleted variables: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrVariableNotFound, name)
	}

	return nil
}

// Values returns the current value of each named variable, keyed by name.
//
// This is the counterpart to SecretRepository.Revisions, and it returns the values
// themselves because that is what a variable contributes to a workload's hash. A
// secret contributes a revision instead, since its hash is reported and one computed
// over a value would confirm a guess at it. A variable's value is reported anyway.
//
// A name that no variable holds is absent from the result rather than an error: the
// caller knows which names it asked about, and deciding what a missing variable means
// is its business. One query rather than one per name, so that hashing a workload
// costs the same however many variables it reads.
func (r *VariableRepository) Values(ctx context.Context, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}

	q := `SELECT name, value FROM variable WHERE name IN (?` + strings.Repeat(", ?", len(names)-1) + `)`

	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query variable values: %w", err)
	}

	values := make(map[string]string, len(names))
	defer rows.Close()

	for rows.Next() {
		var name, value string
		if err = rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("failed to scan variable value: %w", err)
		}

		values[name] = value
	}

	return values, rows.Err()
}

// UsedBy returns the names of the workloads referencing the variable with the given
// name.
//
// Read from the recorded links rather than by matching the reference text inside
// each stored specification, so the answer does not depend on how a reference is
// written or on where in a manifest one is allowed.
//
// A workload being deleted still counts. Its instances run until the reconciler has
// torn them down, so a variable it reads is still in use.
func (r *VariableRepository) UsedBy(ctx context.Context, name string) ([]string, error) {
	const q = `
		SELECT w.name
		FROM workload_variable AS v
		INNER JOIN workload AS w ON w.id = v.workload_id
		WHERE v.variable_name = ?
		ORDER BY w.name ASC
	`

	rows, err := r.db.QueryContext(ctx, q, name)
	if err != nil {
		return nil, fmt.Errorf("failed to query variable users: %w", err)
	}

	var workloads []string
	defer rows.Close()

	for rows.Next() {
		var workload string
		if err = rows.Scan(&workload); err != nil {
			return nil, fmt.Errorf("failed to scan variable user: %w", err)
		}

		workloads = append(workloads, workload)
	}

	return workloads, rows.Err()
}

// linkVariables records which variables a workload references inside an existing
// transaction, so that it can be composed with the write of the workload itself.
//
// Replaced wholesale for the same reason the secret links are: the stored links have
// to describe the specification that was just written, so a reference removed from a
// manifest stops counting as a use.
func linkVariables(ctx context.Context, tx *sql.Tx, workloadID string, names []string) error {
	const (
		clear  = `DELETE FROM workload_variable WHERE workload_id = ?`
		insert = `INSERT INTO workload_variable (workload_id, variable_name) VALUES (?, ?)`
	)

	if _, err := tx.ExecContext(ctx, clear, workloadID); err != nil {
		return fmt.Errorf("failed to clear workload variables: %w", err)
	}

	for _, name := range names {
		if _, err := tx.ExecContext(ctx, insert, workloadID, name); err != nil {
			return fmt.Errorf("failed to record workload variable: %w", err)
		}
	}

	return nil
}
