package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrHostPortTaken is returned when a host port is already allocated to another
	// workload.
	ErrHostPortTaken = errors.New("host port already allocated")
)

type (
	// The Port type represents a host port allocated to a workload.
	//
	// An allocation is desired state: it records the decision that a given host port
	// should reach a given port inside the workload, which is why it is persisted
	// rather than observed. The port a runtime happens to have bound is observed from
	// the driver, and the two agreeing is what the reconciler converges on.
	Port struct {
		// The identifier of the workload the port belongs to.
		WorkloadID string
		// The port the workload listens on inside its runtime.
		Container int
		// The host port that reaches it.
		Host int
		// Whether the host port was allocated by the server rather than pinned by
		// the specification. A dynamic port may be reallocated if it proves
		// unusable; a pinned one may not.
		Dynamic bool
	}

	// The PortRepository type provides persistence operations for the port allocation
	// domain.
	PortRepository struct {
		db *sql.DB
	}
)

// NewPortRepository returns a PortRepository backed by the given database.
func NewPortRepository(db *sql.DB) *PortRepository {
	return &PortRepository{db: db}
}

// List returns the ports allocated to the workload with the given identifier,
// ordered by the port inside the workload so that the result is stable.
func (r *PortRepository) List(ctx context.Context, workloadID string) ([]Port, error) {
	const q = `
		SELECT workload_id, container_port, host_port, is_dynamic
		FROM workload_port
		WHERE workload_id = ?
		ORDER BY container_port ASC
	`

	rows, err := r.db.QueryContext(ctx, q, workloadID)
	if err != nil {
		return nil, fmt.Errorf("failed to query workload ports: %w", err)
	}

	var ports []Port
	defer rows.Close()

	for rows.Next() {
		var port Port
		if err = rows.Scan(&port.WorkloadID, &port.Container, &port.Host, &port.Dynamic); err != nil {
			return nil, fmt.Errorf("failed to scan workload port: %w", err)
		}

		ports = append(ports, port)
	}

	return ports, rows.Err()
}

// ListAll returns the ports allocated to every workload, keyed by workload
// identifier.
//
// This exists so that listing workloads costs one query rather than one per
// workload: the per-workload form is fine for a single read but turns a list of two
// hundred workloads into two hundred round trips.
func (r *PortRepository) ListAll(ctx context.Context) (map[string][]Port, error) {
	const q = `
		SELECT workload_id, container_port, host_port, is_dynamic
		FROM workload_port
		ORDER BY workload_id ASC, container_port ASC
	`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query workload ports: %w", err)
	}

	ports := make(map[string][]Port)
	defer rows.Close()

	for rows.Next() {
		var port Port
		if err = rows.Scan(&port.WorkloadID, &port.Container, &port.Host, &port.Dynamic); err != nil {
			return nil, fmt.Errorf("failed to scan workload port: %w", err)
		}

		ports[port.WorkloadID] = append(ports[port.WorkloadID], port)
	}

	return ports, rows.Err()
}

// Claim records the given ports as allocated to the workload with the given
// identifier, replacing whatever was allocated to it before.
//
// Replacing wholesale rather than merging keeps the stored allocation an exact
// description of the workload's current port list: a port removed from a
// specification releases its allocation as a consequence of no longer being claimed.
// Returns ErrHostPortTaken when one of the host ports is allocated to a different
// workload.
func (r *PortRepository) Claim(ctx context.Context, workloadID string, ports []Port) error {
	const (
		clear  = `DELETE FROM workload_port WHERE workload_id = ?`
		insert = `
			INSERT INTO workload_port (workload_id, container_port, host_port, is_dynamic)
			VALUES (?, ?, ?, ?)
		`
	)

	// The clear and the inserts have to be atomic: a failure between them would
	// otherwise release the workload's ports without claiming its new ones, leaving
	// it reachable on nothing.
	return transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, clear, workloadID); err != nil {
			return fmt.Errorf("failed to clear workload ports: %w", err)
		}

		for _, port := range ports {
			_, err := tx.ExecContext(ctx, insert, workloadID, port.Container, port.Host, port.Dynamic)
			switch {
			case IsUniqueError(err):
				return fmt.Errorf("%w: %d", ErrHostPortTaken, port.Host)
			case err != nil:
				return fmt.Errorf("failed to claim workload port: %w", err)
			}
		}

		return nil
	})
}

// Release removes every port allocated to the workload with the given identifier.
//
// Deleting a workload releases its ports by cascade, so this exists for releasing
// them while the workload itself stays.
func (r *PortRepository) Release(ctx context.Context, workloadID string) error {
	const q = `DELETE FROM workload_port WHERE workload_id = ?`

	if _, err := r.db.ExecContext(ctx, q, workloadID); err != nil {
		return fmt.Errorf("failed to release workload ports: %w", err)
	}

	return nil
}

// HolderOf returns the name of the workload the given host port is allocated to,
// reporting false when no workload holds it.
//
// The name rather than the identifier, because the answer exists to be reported to
// whoever asked for the port: an identifier they never see would tell them nothing.
//
// This is what lets a pinned host port be rejected while the request is still in
// flight, rather than the workload being accepted and then failing to start.
func (r *PortRepository) HolderOf(ctx context.Context, host int) (string, bool, error) {
	const q = `
		SELECT w.name
		FROM workload_port AS p
		INNER JOIN workload AS w ON w.id = p.workload_id
		WHERE p.host_port = ?
	`

	var workload string

	err := r.db.QueryRowContext(ctx, q, host).Scan(&workload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("failed to look up host port: %w", err)
	}

	return workload, true, nil
}

// Allocated returns every host port currently allocated to any workload, which the
// allocator uses to avoid handing out a port orca already promised.
func (r *PortRepository) Allocated(ctx context.Context) ([]int, error) {
	const q = `SELECT host_port FROM workload_port ORDER BY host_port ASC`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query allocated ports: %w", err)
	}

	var ports []int
	defer rows.Close()

	for rows.Next() {
		var port int
		if err = rows.Scan(&port); err != nil {
			return nil, fmt.Errorf("failed to scan allocated port: %w", err)
		}

		ports = append(ports, port)
	}

	return ports, rows.Err()
}
