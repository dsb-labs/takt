package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrServiceNotFound is returned when no service exists with the requested name.
	ErrServiceNotFound = errors.New("service not found")
)

type (
	// The Service type represents a named selection of workload instances to
	// balance requests across.
	//
	// A service is desired state like a workload, but there is nothing running to
	// observe for one: its backends are computed from the workloads its target
	// labels select, so the row holds only the selection.
	Service struct {
		// The identifier the server assigns to the service.
		ID string
		// The name that identifies the service.
		Name string
		// Arbitrary key-value pairs attached to the service.
		Labels map[string]string
		// The labels a workload must carry for its instances to be selected.
		TargetLabels map[string]string
		// The port the selected instances listen on, written as the port inside
		// the workload.
		TargetPort int
		// The transport protocol of the target port.
		TargetProtocol string
		// The time the service was created.
		CreatedAt time.Time
		// The time the service was last modified.
		UpdatedAt time.Time
	}

	// The ServiceRepository type provides persistence operations for the service
	// domain.
	ServiceRepository struct {
		db *sql.DB
	}
)

// NewServiceRepository returns a ServiceRepository backed by the given database.
func NewServiceRepository(db *sql.DB) *ServiceRepository {
	return &ServiceRepository{db: db}
}

// Upsert records the service under its name, replacing what a service already
// holding the name says. The second return value reports whether the service
// was created rather than updated.
//
// An upsert rather than an insert-or-fail, because a service holds no data of
// its own: applying the same manifest twice means the file is the truth, which
// is how a workload's apply already behaves.
func (r *ServiceRepository) Upsert(ctx context.Context, service Service) (Service, bool, error) {
	const q = `
		INSERT INTO service (id, name, labels, target_labels, target_port, target_protocol, created_at, updated_at)
		VALUES (?, ?, jsonb(?), jsonb(?), ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			labels = excluded.labels,
			target_labels = excluded.target_labels,
			target_port = excluded.target_port,
			target_protocol = excluded.target_protocol,
			updated_at = excluded.updated_at
		RETURNING id, created_at, updated_at
	`

	timestamp := formatTime(time.Now().UTC())

	labels, err := marshalLabels(service.Labels)
	if err != nil {
		return Service{}, false, err
	}

	targetLabels, err := marshalLabels(service.TargetLabels)
	if err != nil {
		return Service{}, false, err
	}

	// The identifier is generated ahead of the write. A conflict keeps the
	// existing row's identifier, so the returned one reports which happened.
	id := xid.New().String()

	var createdAt, updatedAt string

	err = r.db.QueryRowContext(ctx, q,
		id, service.Name, labels, targetLabels,
		service.TargetPort, service.TargetProtocol, timestamp, timestamp).
		Scan(&service.ID, &createdAt, &updatedAt)
	if err != nil {
		return Service{}, false, fmt.Errorf("failed to upsert service: %w", err)
	}

	service.CreatedAt, service.UpdatedAt, err = parseTimestamps(createdAt, updatedAt)
	if err != nil {
		return Service{}, false, err
	}

	return service, service.ID == id, nil
}

// Get returns the service with the given name, reporting ErrServiceNotFound when
// no such service exists.
func (r *ServiceRepository) Get(ctx context.Context, name string) (Service, error) {
	const q = `
		SELECT id, name, json(labels), json(target_labels), target_port, target_protocol, created_at, updated_at
		FROM service
		WHERE name = ?
	`

	var (
		service                                    Service
		labels, targetLabels, createdAt, updatedAt string
	)

	err := r.db.QueryRowContext(ctx, q, name).Scan(&service.ID, &service.Name, &labels,
		&targetLabels, &service.TargetPort, &service.TargetProtocol, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Service{}, fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	case err != nil:
		return Service{}, fmt.Errorf("failed to query service: %w", err)
	}

	if err = unmarshalServiceLabels(&service, labels, targetLabels); err != nil {
		return Service{}, err
	}

	service.CreatedAt, service.UpdatedAt, err = parseTimestamps(createdAt, updatedAt)
	if err != nil {
		return Service{}, err
	}

	return service, nil
}

// List returns the services matching every one of the given queries, ordered by
// name so that the result is stable. Passing no queries returns every service.
//
// A query's path reaches the service's labels under $.labels, the same way a
// volume query does. The target labels are not queryable: they say what the
// service selects rather than what it is, and a caller asking what selects a
// workload reads the services back and looks.
func (r *ServiceRepository) List(ctx context.Context, queries ...Query) ([]Service, error) {
	const q = `
		SELECT id, name, json(labels), json(target_labels), target_port, target_protocol, created_at, updated_at
		FROM service
	`

	if err := validPaths(ctx, r.db, queries); err != nil {
		return nil, err
	}

	where, args := filter(labelSource, queries)

	rows, err := r.db.QueryContext(ctx, q+where+"\n\t\tORDER BY name ASC\n\t", args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query services: %w", err)
	}

	var services []Service
	defer rows.Close()

	for rows.Next() {
		var (
			service                                    Service
			labels, targetLabels, createdAt, updatedAt string
		)

		err = rows.Scan(&service.ID, &service.Name, &labels, &targetLabels,
			&service.TargetPort, &service.TargetProtocol, &createdAt, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan service: %w", err)
		}

		if err = unmarshalServiceLabels(&service, labels, targetLabels); err != nil {
			return nil, err
		}

		service.CreatedAt, service.UpdatedAt, err = parseTimestamps(createdAt, updatedAt)
		if err != nil {
			return nil, err
		}

		services = append(services, service)
	}

	return services, rows.Err()
}

// Delete removes the service with the given name, reporting ErrServiceNotFound
// when no such service exists.
func (r *ServiceRepository) Delete(ctx context.Context, name string) error {
	const q = `DELETE FROM service WHERE name = ?`

	result, err := r.db.ExecContext(ctx, q, name)
	if err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to count deleted services: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}

	return nil
}

// unmarshalServiceLabels decodes both of a service row's label columns into it.
func unmarshalServiceLabels(service *Service, labels, targetLabels string) error {
	var err error
	if service.Labels, err = unmarshalLabels(labels); err != nil {
		return err
	}

	service.TargetLabels, err = unmarshalLabels(targetLabels)

	return err
}
