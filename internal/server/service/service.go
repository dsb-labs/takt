package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrServiceNotFound is returned when no service exists with the requested name.
	ErrServiceNotFound = errors.New("service not found")
	// ErrInvalidService is returned when a submitted service fails validation.
	ErrInvalidService = errors.New("invalid service")
)

type (
	// The ServiceRepository interface describes the persistence operations the
	// service service uses.
	ServiceRepository interface {
		// Upsert should record the service under its name, replacing what a
		// service already holding the name says, and report whether it created
		// the row.
		Upsert(ctx context.Context, service database.Service) (database.Service, bool, error)
		// Get should return the service with the given name.
		Get(ctx context.Context, name string) (database.Service, error)
		// List should return the services matching every one of the given
		// queries, or every service when given none.
		List(ctx context.Context, queries ...database.Query) ([]database.Service, error)
		// Delete should remove the service with the given name.
		Delete(ctx context.Context, name string) error
	}

	// The WorkloadLister interface describes how the service service reads the
	// workloads a target selects. Satisfied by the WorkloadService, whose
	// workloads carry their observed instances with health folded into each
	// instance's state and retained instances already removed — which is exactly
	// the question a backend answers.
	WorkloadLister interface {
		// List should return the workloads matching every one of the given
		// "path=value" queries, with observed state merged in.
		List(ctx context.Context, queries ...string) ([]Workload, error)
	}

	// The Backend type describes one address a service balances requests across.
	Backend struct {
		// The name of the workload the instance belongs to.
		Workload string
		// The index of the instance among the workload's instances.
		Instance int
		// The host address that reaches the instance's target port.
		Address string
	}

	// The Service type describes a service as it is reported to a caller.
	Service struct {
		// The name that identifies the service.
		Name string
		// Arbitrary key-value pairs attached to the service.
		Labels map[string]string
		// Which workload instances the service selects, and which of their
		// ports it addresses.
		Target manifest.ServiceTarget
		// The addresses of the selected instances that are fit to serve:
		// running, healthy when checked, and not being torn down.
		Backends []Backend
		// The time the service was created.
		CreatedAt time.Time
		// The time the service was last modified.
		UpdatedAt time.Time
	}

	// The ServiceService type owns services and computes their backends.
	ServiceService struct {
		logger    *slog.Logger
		services  ServiceRepository
		workloads WorkloadLister
		address   string
	}

	// The ServiceServiceConfig type contains fields used to construct a
	// ServiceService.
	ServiceServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the services.
		Services ServiceRepository
		// The source of the workloads a target selects.
		Workloads WorkloadLister
		// The address a backend is reached at, joined with each instance's
		// published host port. The same address workload references resolve to.
		Address string
	}
)

// NewServiceService returns a new instance of the ServiceService type.
func NewServiceService(config ServiceServiceConfig) *ServiceService {
	return &ServiceService{
		logger:    config.Logger.With("component", "service"),
		services:  config.Services,
		workloads: config.Workloads,
		address:   config.Address,
	}
}

// Apply records the service a manifest describes, replacing what a service
// already holding the name says. The second return value reports whether the
// service was created rather than updated.
//
// The manifest is validated again here, though the CLI validates before
// submitting, because a caller of the API is free to skip the CLI.
func (s *ServiceService) Apply(ctx context.Context, spec manifest.Service) (Service, bool, error) {
	spec.Defaults()

	if err := manifest.ValidateService(spec); err != nil {
		return Service{}, false, fmt.Errorf("%w: %v", ErrInvalidService, err)
	}

	stored, created, err := s.services.Upsert(ctx, database.Service{
		Name:           spec.Name,
		Labels:         spec.Labels,
		TargetLabels:   spec.Target.Labels,
		TargetPort:     spec.Target.Port,
		TargetProtocol: string(spec.Target.Protocol),
	})
	if err != nil {
		return Service{}, false, fmt.Errorf("failed to store service: %w", err)
	}

	service, err := s.hydrate(ctx, stored)
	if err != nil {
		return Service{}, false, err
	}

	return service, created, nil
}

// Get returns the service with the given name with its backends resolved,
// reporting ErrServiceNotFound when no such service exists.
func (s *ServiceService) Get(ctx context.Context, name string) (Service, error) {
	stored, err := s.services.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrServiceNotFound):
		return Service{}, fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	case err != nil:
		return Service{}, fmt.Errorf("failed to get service: %w", err)
	}

	return s.hydrate(ctx, stored)
}

// List returns the services matching every one of the given queries with their
// backends resolved. Passing no queries returns every service.
//
// Each query is a "path=value" string reaching the service's labels under
// $.labels, as a workload query does. Returns ErrInvalidQuery when one is
// malformed.
func (s *ServiceService) List(ctx context.Context, queries ...string) ([]Service, error) {
	parsed, err := parseQueries(queries)
	if err != nil {
		return nil, err
	}

	rows, err := s.services.List(ctx, parsed...)
	if err != nil {
		if errors.Is(err, database.ErrInvalidQueryPath) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
		}

		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	services := make([]Service, 0, len(rows))
	for _, row := range rows {
		service, err := s.hydrate(ctx, row)
		if err != nil {
			return nil, err
		}

		services = append(services, service)
	}

	return services, nil
}

// Delete removes the service with the given name, reporting ErrServiceNotFound
// when no such service exists.
//
// Only the row. The workloads the service selected are their own resources and
// keep running; what stops is the service reporting their addresses.
func (s *ServiceService) Delete(ctx context.Context, name string) error {
	err := s.services.Delete(ctx, name)
	switch {
	case errors.Is(err, database.ErrServiceNotFound):
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	case err != nil:
		return fmt.Errorf("failed to delete service: %w", err)
	default:
		return nil
	}
}

// hydrate maps a stored service row to the caller-facing type, resolving its
// backends.
func (s *ServiceService) hydrate(ctx context.Context, row database.Service) (Service, error) {
	backends, err := s.backends(ctx, row)
	if err != nil {
		return Service{}, err
	}

	return Service{
		Name:   row.Name,
		Labels: row.Labels,
		Target: manifest.ServiceTarget{
			Labels:   row.TargetLabels,
			Port:     row.TargetPort,
			Protocol: manifest.Protocol(row.TargetProtocol),
		},
		Backends:  backends,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// backends resolves the addresses of the instances the service's target
// selects that are fit to serve.
//
// A workload is selected when it carries every target label. An instance
// counts when it is observed running — health is folded into the observed
// state, so an instance failing its check has already left that state, and a
// workload with no check contributes its running instances as they are. A
// workload being deleted contributes nothing: its instances keep serving until
// the teardown reaches them, but a balancer told about them would keep sending
// requests to addresses about to vanish.
func (s *ServiceService) backends(ctx context.Context, row database.Service) ([]Backend, error) {
	selected, err := s.workloads.List(ctx, targetQueries(row.TargetLabels)...)
	if err != nil {
		return nil, fmt.Errorf("failed to list targeted workloads: %w", err)
	}

	var backends []Backend
	for _, workload := range selected {
		if workload.Deleting {
			continue
		}

		for _, instance := range workload.Instances {
			if instance.State != driver.StateRunning {
				continue
			}

			port, ok := targetPort(workload.Ports, row, instance.Index)
			if !ok {
				continue
			}

			backends = append(backends, Backend{
				Workload: workload.Name,
				Instance: instance.Index,
				Address:  net.JoinHostPort(s.address, strconv.Itoa(port.From)),
			})
		}
	}

	// Ordered so that the same fleet always reads back the same way, whatever
	// order the workloads and instances were observed in.
	slices.SortFunc(backends, func(a, b Backend) int {
		if a.Workload != b.Workload {
			return cmp.Compare(a.Workload, b.Workload)
		}

		return cmp.Compare(a.Instance, b.Instance)
	})

	return backends, nil
}

// targetPort finds the instance's host port mapping for the service's target
// port and protocol, reporting false when the workload does not publish it.
//
// A selected workload that does not publish the target port contributes
// nothing rather than failing the service: a label selector is allowed to
// span workloads where only some publish it.
func targetPort(ports []ResolvedPort, row database.Service, instance int) (ResolvedPort, bool) {
	for _, port := range ports {
		if port.Instance == instance && port.To == row.TargetPort &&
			string(port.Protocol) == row.TargetProtocol {
			return port, true
		}
	}

	return ResolvedPort{}, false
}

// targetQueries builds the workload list queries that select every workload
// carrying the target's labels. Keys are visited in sorted order so the same
// target always asks the same question.
//
// Each key is quoted in the JSON path because label keys may contain dots,
// which would otherwise read as path separators. A key cannot contain a quote,
// so the quoting cannot be escaped.
func targetQueries(labels map[string]string) []string {
	queries := make([]string, 0, len(labels))
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		queries = append(queries, fmt.Sprintf(`$.labels."%s"=%s`, key, labels[key]))
	}

	return queries
}
