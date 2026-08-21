package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		// List should return the workloads matching every one of the given queries,
		// or all of them when none are given.
		List(ctx context.Context, queries ...string) ([]service.Workload, error)
		// Delete should mark the workload with the given name for deletion,
		// returning it as it now stands.
		Delete(ctx context.Context, name string) (service.Workload, error)
		// Logs should write the recent output of the named workload to out.
		Logs(ctx context.Context, out io.Writer, name string, tail int) error
	}

	// The WorkloadAPI type exposes HTTP endpoints for managing workloads.
	WorkloadAPI struct {
		logger    *slog.Logger
		workloads WorkloadService
	}

	// The WorkloadAPIConfig type contains fields used to construct a WorkloadAPI.
	WorkloadAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service performing the workload operations.
		Workloads WorkloadService
	}
)

const (
	// The default number of log lines returned when a request doesn't ask for a
	// specific number. Matches the default declared in the specification.
	defaultLogTail = 100
	// The most log lines a request may ask for. The specification declares this
	// maximum, but the generated code does not enforce it, and the server reads what
	// it is asked to read — so an uncapped request would let a caller decide how much
	// work the server does.
	maxLogTail = 10000
)

// NewWorkloadAPI returns a new instance of the WorkloadAPI type.
func NewWorkloadAPI(config WorkloadAPIConfig) *WorkloadAPI {
	return &WorkloadAPI{
		logger:    config.Logger.With("component", "api"),
		workloads: config.Workloads,
	}
}

// internalError logs why a request failed and returns the message the client is told
// instead.
//
// An unexpected failure is described to the operator, not to the caller: the error
// carries whatever context it was wrapped with on the way up — a filesystem path, the
// text of a query, a docker endpoint — and none of that is the caller's business or
// safe to hand them.
func (a *WorkloadAPI) internalError(operation string, err error) string {
	a.logger.With("error", err, "operation", operation).Error("failed to serve request")

	return "failed to " + operation
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
	case errors.Is(err, service.ErrNoPortsAvailable):
		// The request is valid and will be servable once a port frees up, which is
		// what distinguishes this from a client error: nothing about the manifest
		// needs to change.
		return api.ApplyWorkload503JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrUnsupportedRuntime):
		return api.ApplyWorkload422JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrVolumeNotFound),
		errors.Is(err, service.ErrInvalidSpec),
		errors.Is(err, service.ErrNoRuntime),
		errors.Is(err, service.ErrAmbiguousRuntime):
		// A volume that does not exist is the caller's to fix, and naming it is the
		// whole point: the alternative is an operator who mistyped a volume being told
		// only that something went wrong.
		return api.ApplyWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case err != nil:
		return api.ApplyWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("apply workload", err),
			},
		}, nil
	}

	if created {
		return api.ApplyWorkload201JSONResponse{Workload: newWorkload(workload)}, nil
	}

	return api.ApplyWorkload200JSONResponse{Workload: newWorkload(workload)}, nil
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
				Error: a.internalError("get workload", err),
			},
		}, nil
	}

	return api.GetWorkload200JSONResponse{Workload: newWorkload(workload)}, nil
}

// ListWorkloads returns the workloads matching the request's queries, or every
// workload when it carries none.
func (a *WorkloadAPI) ListWorkloads(ctx context.Context, request api.ListWorkloadsRequestObject) (api.ListWorkloadsResponseObject, error) {
	var queries []string
	if request.Params.Query != nil {
		queries = *request.Params.Query
	}

	workloads, err := a.workloads.List(ctx, queries...)
	switch {
	case errors.Is(err, service.ErrInvalidQuery):
		return api.ListWorkloads400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case err != nil:
		return api.ListWorkloads500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("list workloads", err),
			},
		}, nil
	}

	response := api.ListWorkloads200JSONResponse{Workloads: make([]api.Workload, 0, len(workloads))}
	for _, workload := range workloads {
		response.Workloads = append(response.Workloads, newWorkload(workload))
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
				Error: a.internalError("delete workload", err),
			},
		}, nil
	}

	return api.DeleteWorkload202JSONResponse{Workload: newWorkload(workload)}, nil
}

// GetWorkloadLogs returns the recent output of the named workload.
func (a *WorkloadAPI) GetWorkloadLogs(ctx context.Context, request api.GetWorkloadLogsRequestObject) (api.GetWorkloadLogsResponseObject, error) {
	tail := defaultLogTail
	if request.Params.Tail != nil {
		tail = min(*request.Params.Tail, maxLogTail)
	}

	// A missing workload is established before anything is written, because once the
	// first byte of a 200 has been sent there is no way to report a failure. Reading
	// the logs can still fail midway through; nothing can be done about that but stop
	// writing, which is exactly why the cheap check happens first.
	if _, err := a.workloads.Get(ctx, request.Name); err != nil {
		switch {
		case errors.Is(err, service.ErrWorkloadNotFound):
			return api.GetWorkloadLogs404JSONResponse{
				NotFoundJSONResponse: api.NotFoundJSONResponse{
					Error: fmt.Sprintf("workload %q does not exist", request.Name),
				},
			}, nil
		default:
			return api.GetWorkloadLogs500JSONResponse{
				InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
					Error: a.internalError("read workload logs", err),
				},
			}, nil
		}
	}

	return logsResponse{
		write: func(w io.Writer) error {
			return a.workloads.Logs(ctx, w, request.Name, tail)
		},
	}, nil
}

// The logsResponse type streams a workload's logs to the client.
//
// The generated response type for this endpoint is a string, which would mean holding
// the whole of a workload's output in memory before sending any of it. This writes
// straight to the response instead, so the server's memory use doesn't scale with how
// much a container has to say.
type logsResponse struct {
	write func(w io.Writer) error
}

// VisitGetWorkloadLogsResponse writes the logs to w as plain text.
func (r logsResponse) VisitGetWorkloadLogsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	return r.write(w)
}

// instanceHealth maps what orca established about a workload's health onto the wire
// format, falling back to what the runtime reports for an image that declares its own
// check.
//
// The check orca performs takes precedence: it is the one the operator asked for,
// where the runtime's is whatever the image happened to carry.
func instanceHealth(reported service.Health, instance driver.Instance) *api.InstanceHealth {
	if reported.Checked {
		result := api.InstanceHealth{Status: api.HealthStatus(reported.Result.Status)}

		result.Failures = new(reported.Result.Failures)

		if !reported.Result.CheckedAt.IsZero() {
			result.CheckedAt = new(reported.Result.CheckedAt)
		}
		if reported.Result.Error != "" {
			result.Error = new(reported.Result.Error)
		}

		return &result
	}

	if instance.RuntimeHealth == "" {
		return nil
	}

	// Reported but not acted on: orca did not ask for this check and has no retry
	// policy for it, so surfacing it is strictly more useful than discarding it.
	return &api.InstanceHealth{Status: api.HealthStatus(instance.RuntimeHealth)}
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

	if !w.NextRun.IsZero() {
		workload.NextRun = new(w.NextRun)
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
			Health:   instanceHealth(w.Health, instance),
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
