package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Service type is the client-side view of a service: a named selection
	// of workload instances to balance requests across.
	Service struct {
		// The name that identifies the service.
		Name string
		// Arbitrary key-value pairs attached to the service.
		Labels map[string]string
		// Which workload instances the service selects, and which of their
		// ports it addresses.
		Target manifest.ServiceTarget
		// The selected instances that are fit to serve when the response was
		// written. Empty when nothing selected is fit to serve.
		Backends []ServiceBackend
		// The time the service was created.
		CreatedAt time.Time
		// The time the service was last modified.
		UpdatedAt time.Time
	}

	// The ServiceBackend type is one address a service balances requests
	// across.
	ServiceBackend struct {
		// The name of the workload the instance belongs to.
		Workload string
		// The index of the instance among the workload's instances.
		Instance int
		// The host address that reaches the instance's target port, as
		// "host:port".
		Address string
	}
)

// checkServiceName reports whether a name is usable as a single segment of a
// request path, for the reason checkName does.
func checkServiceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q is not a single path segment", ErrInvalidServiceName, name)
	}

	return nil
}

// ApplyService submits the service as desired state, returning it as it now
// stands with its backends resolved.
//
// The operation is idempotent: the stored selection becomes what the manifest
// says, however many times it is applied. The workloads the target selects do
// not have to exist, so a service applied ahead of its workloads reports no
// backends until they arrive.
func (c *Client) ApplyService(ctx context.Context, service manifest.Service) (Service, error) {
	if err := checkServiceName(service.Name); err != nil {
		return Service{}, err
	}

	resp, err := c.api.ApplyServiceWithResponse(ctx, service.Name, wire.FromService(service))
	if err != nil {
		return Service{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newService(resp.JSON200.Service), nil
	case resp.JSON201 != nil:
		return newService(resp.JSON201.Service), nil
	case resp.JSON400 != nil:
		return Service{}, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return Service{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Service{}, newError(resp.StatusCode(), nil)
	}
}

// GetService returns the service with the given name with its backends
// resolved, returning ErrServiceNotFound when no such service exists.
func (c *Client) GetService(ctx context.Context, name string) (Service, error) {
	if err := checkServiceName(name); err != nil {
		return Service{}, err
	}

	resp, err := c.api.GetServiceWithResponse(ctx, name)
	if err != nil {
		return Service{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newService(resp.JSON200.Service), nil
	case resp.JSON404 != nil:
		return Service{}, fmt.Errorf("%s: %w", resp.JSON404.Error, ErrServiceNotFound)
	case resp.JSON500 != nil:
		return Service{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Service{}, newError(resp.StatusCode(), nil)
	}
}

// ListServices returns the services matching every one of the given queries, or
// all of them when none are given.
//
// A query is a "path=value" filter over the service's labels, where the path is
// a JSON path such as "$.labels.app". A query can reach only the service's own
// labels, not its target's.
func (c *Client) ListServices(ctx context.Context, queries ...string) ([]Service, error) {
	var params api.ListServicesParams
	if len(queries) > 0 {
		params.Query = &queries
	}

	resp, err := c.api.ListServicesWithResponse(ctx, &params)
	if err != nil {
		return nil, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		services := make([]Service, 0, len(resp.JSON200.Services))
		for _, listed := range resp.JSON200.Services {
			services = append(services, newService(listed))
		}

		return services, nil
	case resp.JSON400 != nil:
		return nil, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return nil, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return nil, newError(resp.StatusCode(), nil)
	}
}

// DeleteService removes the service with the given name, returning
// ErrServiceNotFound when no such service exists.
//
// The workloads the service selected keep running. What stops is the service
// reporting their addresses.
func (c *Client) DeleteService(ctx context.Context, name string) error {
	if err := checkServiceName(name); err != nil {
		return err
	}

	resp, err := c.api.DeleteServiceWithResponse(ctx, name)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON404 != nil:
		return fmt.Errorf("%s: %w", resp.JSON404.Error, ErrServiceNotFound)
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

// newService maps a service as the API reports it onto the client's own shape.
func newService(service api.Service) Service {
	out := Service{
		Name: service.Name,
		Target: manifest.ServiceTarget{
			Labels: service.Target.Labels,
			Port:   service.Target.Port,
		},
		CreatedAt: service.CreatedAt,
		UpdatedAt: service.UpdatedAt,
	}

	if service.Target.Protocol != nil {
		out.Target.Protocol = manifest.Protocol(*service.Target.Protocol)
	}

	if service.Labels != nil {
		out.Labels = *service.Labels
	}

	if service.Backends != nil {
		out.Backends = make([]ServiceBackend, 0, len(*service.Backends))
		for _, backend := range *service.Backends {
			out.Backends = append(out.Backends, ServiceBackend{
				Workload: backend.Workload,
				Instance: backend.Instance,
				Address:  backend.Address,
			})
		}
	}

	return out
}
