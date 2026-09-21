package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrServiceNotFound is returned when no service exists with the requested name.
	ErrServiceNotFound = errors.New("service not found")
	// ErrServiceChanged is returned when the service an apply was conditioned on is
	// not the service the database holds.
	ErrServiceChanged = errors.New("service changed since it was read")
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
		// How many times the service has been written. This is the entity tag a
		// conditional apply compares against, and it moves only when an apply
		// changed something.
		Version int
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
//
// A non-zero ifMatch conditions the write on the stored version still being
// that one, reporting ErrServiceChanged when it is not and ErrServiceNotFound
// when there is no row to have the version at all. Zero applies
// unconditionally, which is what a caller creating a service has to do: there
// is no version yet to name.
//
// Like a workload's apply, a write that would change nothing is skipped, so
// re-applying an unchanged manifest leaves the version where it is. That is
// what lets a pipeline apply the same file repeatedly without invalidating
// the tag it is holding.
func (r *ServiceRepository) Upsert(ctx context.Context, service Service, ifMatch int) (Service, bool, error) {
	labels, err := marshalLabels(service.Labels)
	if err != nil {
		return Service{}, false, err
	}

	targetLabels, err := marshalLabels(service.TargetLabels)
	if err != nil {
		return Service{}, false, err
	}

	var (
		stored  Service
		created bool
	)

	err = transaction(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		existing, err := getService(ctx, tx, service.Name)
		switch {
		case errors.Is(err, ErrServiceNotFound):
			if ifMatch != 0 {
				return err
			}

			stored, err = insertService(ctx, tx, service, labels, targetLabels)

			created = true

			return err
		case err != nil:
			return err
		case ifMatch != 0 && existing.Version != ifMatch:
			return ErrServiceChanged
		case unchangedService(existing, service):
			stored = existing

			return nil
		}

		stored, err = updateService(ctx, tx, service, labels, targetLabels, existing)

		return err
	})
	if err != nil {
		return Service{}, false, err
	}

	return stored, created, nil
}

// insertService writes a service that does not exist yet.
func insertService(ctx context.Context, tx *sql.Tx, service Service, labels, targetLabels string) (Service, error) {
	const q = `
		INSERT INTO service (id, name, labels, target_labels, target_port, target_protocol, version, created_at, updated_at)
		VALUES (?, ?, jsonb(?), jsonb(?), ?, ?, 1, ?, ?)
	`

	now := time.Now().UTC()
	timestamp := formatTime(now)

	service.ID = xid.New().String()

	_, err := tx.ExecContext(ctx, q, service.ID, service.Name, labels, targetLabels,
		service.TargetPort, service.TargetProtocol, timestamp, timestamp)
	if err != nil {
		return Service{}, fmt.Errorf("failed to insert service: %w", err)
	}

	service.Version = 1
	service.CreatedAt = now
	service.UpdatedAt = now

	return service, nil
}

// updateService replaces the fields an apply owns, moving the version on.
func updateService(ctx context.Context, tx *sql.Tx, service Service, labels, targetLabels string, existing Service) (Service, error) {
	const q = `
		UPDATE service
		SET labels = jsonb(?), target_labels = jsonb(?), target_port = ?,
			target_protocol = ?, version = ?, updated_at = ?
		WHERE name = ?
	`

	now := time.Now().UTC()

	service.ID = existing.ID
	service.Version = existing.Version + 1
	service.CreatedAt = existing.CreatedAt
	service.UpdatedAt = now

	_, err := tx.ExecContext(ctx, q, labels, targetLabels, service.TargetPort,
		service.TargetProtocol, service.Version, formatTime(now), service.Name)
	if err != nil {
		return Service{}, fmt.Errorf("failed to update service: %w", err)
	}

	return service, nil
}

// unchangedService reports whether an apply would leave the stored service
// exactly as it is, which is the case where the write is skipped.
func unchangedService(existing, incoming Service) bool {
	return existing.TargetPort == incoming.TargetPort &&
		existing.TargetProtocol == incoming.TargetProtocol &&
		maps.Equal(existing.Labels, incoming.Labels) &&
		maps.Equal(existing.TargetLabels, incoming.TargetLabels)
}

// Get returns the service with the given name, reporting ErrServiceNotFound when
// no such service exists.
func (r *ServiceRepository) Get(ctx context.Context, name string) (Service, error) {
	return getService(ctx, r.db, name)
}

// getService reads a service through anything that can run a query, so that the
// conditional write can read under the same lock it goes on to write with.
func getService(ctx context.Context, q querier, name string) (Service, error) {
	const stmt = `
		SELECT id, name, json(labels), json(target_labels), target_port, target_protocol, version, created_at, updated_at
		FROM service
		WHERE name = ?
	`

	var (
		service                                    Service
		labels, targetLabels, createdAt, updatedAt string
	)

	err := q.QueryRowContext(ctx, stmt, name).Scan(&service.ID, &service.Name, &labels,
		&targetLabels, &service.TargetPort, &service.TargetProtocol, &service.Version, &createdAt, &updatedAt)
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
		SELECT id, name, json(labels), json(target_labels), target_port, target_protocol, version, created_at, updated_at
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
			&service.TargetPort, &service.TargetProtocol, &service.Version, &createdAt, &updatedAt)
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
