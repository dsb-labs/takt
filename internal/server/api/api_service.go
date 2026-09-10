package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The ServiceService interface describes the service operations the API
	// exposes.
	ServiceService interface {
		// Apply should record the service a manifest describes, replacing what a
		// service already holding the name says, and report whether it created
		// the service.
		Apply(ctx context.Context, spec manifest.Service) (service.Service, bool, error)
		// Get should return the service with the given name with its backends
		// resolved.
		Get(ctx context.Context, name string) (service.Service, error)
		// List should return the services matching every one of the given
		// "path=value" queries, or every service the server holds when given
		// none.
		List(ctx context.Context, queries ...string) ([]service.Service, error)
		// Delete should remove the service with the given name.
		Delete(ctx context.Context, name string) error
	}

	// The ServiceAPI type exposes HTTP endpoints for managing services.
	ServiceAPI struct {
		logger   *slog.Logger
		services ServiceService
	}

	// The ServiceAPIConfig type contains fields used to construct a ServiceAPI.
	ServiceAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service performing the service operations.
		Services ServiceService
	}
)

// NewServiceAPI returns a new instance of the ServiceAPI type.
func NewServiceAPI(config ServiceAPIConfig) *ServiceAPI {
	return &ServiceAPI{
		logger:   config.Logger.With("component", "api"),
		services: config.Services,
	}
}

// ApplyService records the submitted specification as the desired state for the
// named service.
func (a *ServiceAPI) ApplyService(ctx context.Context, request api.ApplyServiceRequestObject) (api.ApplyServiceResponseObject, error) {
	if request.Body == nil {
		return api.ApplyService400JSONResponse{
			Error: "request body is required",
		}, nil
	}

	// The path is where the resource's identity lives, so a body naming something
	// else is a mistake worth reporting rather than quietly resolving either way.
	if request.Body.Name != request.Name {
		return api.ApplyService400JSONResponse{
			Error: fmt.Sprintf("the specification names %q but the path names %q", request.Body.Name, request.Name),
		}, nil
	}

	applied, created, err := a.services.Apply(ctx, wire.ToService(*request.Body))
	switch {
	case errors.Is(err, service.ErrInvalidService):
		return api.ApplyService400JSONResponse{
			Error: err.Error(),
		}, nil
	case err != nil:
		return api.ApplyService500JSONResponse{
			Error: internalError(a.logger, "apply service", err),
		}, nil
	}

	if created {
		return api.ApplyService201JSONResponse{Service: newService(applied)}, nil
	}

	return api.ApplyService200JSONResponse{Service: newService(applied)}, nil
}

// GetService returns the service with the given name.
func (a *ServiceAPI) GetService(ctx context.Context, request api.GetServiceRequestObject) (api.GetServiceResponseObject, error) {
	got, err := a.services.Get(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrServiceNotFound):
		return api.GetService404JSONResponse{
			Error: fmt.Sprintf("service %q does not exist", request.Name),
		}, nil
	case err != nil:
		return api.GetService500JSONResponse{
			Error: internalError(a.logger, "get service", err),
		}, nil
	}

	return api.GetService200JSONResponse{Service: newService(got)}, nil
}

// ListServices returns the services matching the request's queries, or every
// service when it carries none.
func (a *ServiceAPI) ListServices(ctx context.Context, request api.ListServicesRequestObject) (api.ListServicesResponseObject, error) {
	var queries []string
	if request.Params.Query != nil {
		queries = *request.Params.Query
	}

	services, err := a.services.List(ctx, queries...)
	switch {
	case errors.Is(err, service.ErrInvalidQuery):
		return api.ListServices400JSONResponse{
			Error: err.Error(),
		}, nil
	case err != nil:
		return api.ListServices500JSONResponse{
			Error: internalError(a.logger, "list services", err),
		}, nil
	}

	response := api.ListServices200JSONResponse{Services: make([]api.Service, 0, len(services))}
	for _, listed := range services {
		response.Services = append(response.Services, newService(listed))
	}

	return response, nil
}

// DeleteService removes the service with the given name.
//
// The response is 200 rather than 202, unlike deleting a workload: the workloads
// the service selected are their own resources and keep running, so there is
// nothing to wind down.
func (a *ServiceAPI) DeleteService(ctx context.Context, request api.DeleteServiceRequestObject) (api.DeleteServiceResponseObject, error) {
	err := a.services.Delete(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrServiceNotFound):
		return api.DeleteService404JSONResponse{
			Error: fmt.Sprintf("service %q does not exist", request.Name),
		}, nil
	case err != nil:
		return api.DeleteService500JSONResponse{
			Error: internalError(a.logger, "delete service", err),
		}, nil
	}

	return api.DeleteService200JSONResponse{}, nil
}

// newService converts a service as the service layer reports it into its wire
// representation.
func newService(s service.Service) api.Service {
	protocol := api.ServiceTargetProtocol(s.Target.Protocol)

	out := api.Service{
		Name: s.Name,
		Target: api.ServiceTarget{
			Labels:   s.Target.Labels,
			Port:     s.Target.Port,
			Protocol: &protocol,
		},
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}

	// Absent rather than an empty array when nothing is fit to serve, so that
	// "no backends" and "not reported" are not the same value on the wire.
	if len(s.Backends) > 0 {
		backends := make([]api.ServiceBackend, 0, len(s.Backends))
		for _, backend := range s.Backends {
			backends = append(backends, api.ServiceBackend{
				Workload: backend.Workload,
				Instance: backend.Instance,
				Address:  backend.Address,
			})
		}

		out.Backends = &backends
	}

	out.Labels = wireLabels(s.Labels)

	return out
}
