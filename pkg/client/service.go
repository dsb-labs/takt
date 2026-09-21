package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		// The entity tag identifying this version of the service, which a conditional
		// apply hands back in WithIfMatch. Empty on one read from a list, which
		// reports no tag per item.
		ETag string
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
func (c *Client) ApplyService(ctx context.Context, service manifest.Service, options ...ApplyOption) (Service, error) {
	if err := checkServiceName(service.Name); err != nil {
		return Service{}, err
	}

	resp, err := c.api.ApplyServiceWithResponse(ctx, service.Name, &api.ApplyServiceParams{IfMatch: ifMatch(options)}, wire.FromService(service))
	if err != nil {
		return Service{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		applied := newService(resp.JSON200.Service)
		applied.ETag = resp.HTTPResponse.Header.Get("ETag")

		return applied, nil
	case resp.JSON201 != nil:
		applied := newService(resp.JSON201.Service)
		applied.ETag = resp.HTTPResponse.Header.Get("ETag")

		return applied, nil
	case resp.JSON412 != nil:
		return Service{}, fmt.Errorf("%s: %w", resp.JSON412.Error, ErrServiceChanged)
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
		got := newService(resp.JSON200.Service)
		got.ETag = resp.HTTPResponse.Header.Get("ETag")

		return got, nil
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

// StreamServices calls fn with the services matching every one of the given
// queries as soon as the server answers, and again each time the set changes,
// until ctx is cancelled, fn returns an error, or the server ends the stream.
//
// Each call carries the whole set, so fn replaces what it knows rather than
// applying a change to it: a service or backend that has gone is absent from
// the next call. Cancellation is not reported as a failure, since it is how a
// caller ends a stream. A server ending it is reported as nil too, and a caller
// that wants to keep following opens a new stream and starts again from the
// first set.
func (c *Client) StreamServices(ctx context.Context, fn func([]Service) error, queries ...string) error {
	params := api.ListServicesParams{Follow: new(true)}
	if len(queries) > 0 {
		params.Query = &queries
	}

	// A stream legitimately outlives the client's timeout, so it goes out over the
	// client that has none. What ends it is the caller's context.
	resp, err := c.stream.ListServices(ctx, &params)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := errorBody(resp)

		return newError(resp.StatusCode, &body)
	}

	decoder := json.NewDecoder(resp.Body)
	for {
		var line api.ListServicesResult

		err = decoder.Decode(&line)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil && ctx.Err() != nil:
			// A caller who cancelled already knows why the stream stopped, and the
			// read fails in whatever way the transport noticed first.
			return nil
		case err != nil:
			return fmt.Errorf("failed to read the response body: %w", err)
		}

		services := make([]Service, 0, len(line.Services))
		for _, listed := range line.Services {
			services = append(services, newService(listed))
		}

		if err = fn(services); err != nil {
			return err
		}
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
