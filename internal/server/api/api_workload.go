package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/service"
)

type (
	// The WorkloadService interface describes the workload operations the API exposes.
	WorkloadService interface {
		// Apply should store the given specification as desired state, reporting
		// whether the workload was newly created.
		Apply(ctx context.Context, spec api.WorkloadSpec) (service.Workload, bool, error)
		// Get should return the workload with the given name.
		Get(ctx context.Context, name string) (service.Workload, error)
		// List should return every workload.
		List(ctx context.Context) ([]service.Workload, error)
		// Delete should mark the workload with the given name for deletion,
		// returning it as it now stands.
		Delete(ctx context.Context, name string) (service.Workload, error)
		// Logs should return the recent output of the named workload.
		Logs(ctx context.Context, name string, tail int) (string, error)
	}

	// The WorkloadAPI type exposes HTTP endpoints for managing workloads.
	WorkloadAPI struct {
		workloads WorkloadService
	}
)

// The default number of log lines returned when a request doesn't ask for a
// specific number. Matches the default declared in the specification.
const defaultLogTail = 100

// NewWorkloadAPI returns a new instance of the WorkloadAPI type.
func NewWorkloadAPI(workloads WorkloadService) *WorkloadAPI {
	return &WorkloadAPI{workloads: workloads}
}

// Register the HTTP endpoints onto the given http.ServeMux.
//
// The routes themselves come from the generated handler, which is mounted onto the
// caller's mux rather than one of its own so that the server keeps ownership of
// routing and can wrap the whole surface in its own middleware.
func (a *WorkloadAPI) Register(mux *http.ServeMux) {
	api.HandlerWithOptions(api.NewStrictHandler(a, nil), api.StdHTTPServerOptions{
		BaseRouter: mux,
	})
}

// ApplyWorkload stores the given specification as the desired state for the named
// workload.
func (a *WorkloadAPI) ApplyWorkload(ctx context.Context, request api.ApplyWorkloadRequestObject) (api.ApplyWorkloadResponseObject, error) {
	if request.Body == nil {
		return api.ApplyWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: "request body is required"},
		}, nil
	}

	spec := *request.Body

	// The name appears in both the path and the body, so a mismatch is ambiguous
	// rather than something to silently resolve in favour of either one.
	if spec.Name != request.Name {
		return api.ApplyWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{
				Error: fmt.Sprintf("workload name %q does not match name %q in the request path", spec.Name, request.Name),
			},
		}, nil
	}

	workload, created, err := a.workloads.Apply(ctx, spec)
	switch {
	case errors.Is(err, service.ErrWorkloadDeleting):
		return api.ApplyWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is being deleted", request.Name),
		}, nil
	case errors.Is(err, service.ErrHostPortTaken):
		// A pinned host port another workload holds is a conflict with existing
		// state rather than a malformed request, and the message names the holder
		// so the fix is obvious.
		return api.ApplyWorkload409JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrUnsupportedRuntime):
		return api.ApplyWorkload422JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrNoRuntime), errors.Is(err, service.ErrAmbiguousRuntime):
		return api.ApplyWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case err != nil:
		return api.ApplyWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: fmt.Sprintf("failed to apply workload: %v", err),
			},
		}, nil
	}

	if created {
		return api.ApplyWorkload201JSONResponse(newWorkload(workload)), nil
	}

	return api.ApplyWorkload200JSONResponse(newWorkload(workload)), nil
}

// GetWorkload returns the workload with the given name.
func (a *WorkloadAPI) GetWorkload(ctx context.Context, request api.GetWorkloadRequestObject) (api.GetWorkloadResponseObject, error) {
	workload, err := a.workloads.Get(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.GetWorkload404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case err != nil:
		return api.GetWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: fmt.Sprintf("failed to get workload: %v", err),
			},
		}, nil
	}

	return api.GetWorkload200JSONResponse(newWorkload(workload)), nil
}

// ListWorkloads returns every workload known to the server.
func (a *WorkloadAPI) ListWorkloads(ctx context.Context, _ api.ListWorkloadsRequestObject) (api.ListWorkloadsResponseObject, error) {
	workloads, err := a.workloads.List(ctx)
	if err != nil {
		return api.ListWorkloads500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: fmt.Sprintf("failed to list workloads: %v", err),
			},
		}, nil
	}

	response := make(api.ListWorkloads200JSONResponse, 0, len(workloads))
	for _, workload := range workloads {
		response = append(response, newWorkload(workload))
	}

	return response, nil
}

// DeleteWorkload marks the workload with the given name for deletion.
//
// The response is 202 rather than 204 because the workload is not gone when the
// request returns: it is reported as terminating until the driver's work for it has
// actually stopped, at which point it disappears.
func (a *WorkloadAPI) DeleteWorkload(ctx context.Context, request api.DeleteWorkloadRequestObject) (api.DeleteWorkloadResponseObject, error) {
	workload, err := a.workloads.Delete(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.DeleteWorkload404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case err != nil:
		return api.DeleteWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: fmt.Sprintf("failed to delete workload: %v", err),
			},
		}, nil
	}

	return api.DeleteWorkload202JSONResponse(newWorkload(workload)), nil
}

// GetWorkloadLogs returns the recent output of the named workload.
func (a *WorkloadAPI) GetWorkloadLogs(ctx context.Context, request api.GetWorkloadLogsRequestObject) (api.GetWorkloadLogsResponseObject, error) {
	tail := defaultLogTail
	if request.Params.Tail != nil {
		tail = *request.Params.Tail
	}

	logs, err := a.workloads.Logs(ctx, request.Name, tail)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.GetWorkloadLogs404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case err != nil:
		return api.GetWorkloadLogs500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: fmt.Sprintf("failed to read workload logs: %v", err),
			},
		}, nil
	}

	return api.GetWorkloadLogs200TextResponse(logs), nil
}

// newWorkload maps the service's view of a workload onto the wire format.
func newWorkload(w service.Workload) api.Workload {
	workload := api.Workload{
		Name:      w.Name,
		Version:   w.Version,
		Runtime:   w.Runtime,
		State:     w.State,
		Spec:      w.Spec,
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}

	if w.Deleting {
		workload.Deleting = new(true)
	}

	if len(w.Ports) > 0 {
		workload.Ports = new(w.Ports)
	}

	if len(w.Instances) == 0 {
		return workload
	}

	instances := make([]api.Instance, 0, len(w.Instances))
	for _, instance := range w.Instances {
		mapped := api.Instance{
			ID:       instance.ID,
			SpecHash: instance.SpecHash,
			State:    api.InstanceState(instance.State),
		}

		// An exit code is only meaningful once the instance has stopped; reporting
		// zero for something still running would read as a clean exit.
		if instance.State == driver.StateExited || instance.State == driver.StateFailed {
			mapped.ExitCode = new(instance.ExitCode)
		}

		if !instance.StartedAt.IsZero() {
			mapped.StartedAt = new(instance.StartedAt)
		}

		instances = append(instances, mapped)
	}

	workload.Instances = &instances

	return workload
}
