package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/service"
	"github.com/dsb-labs/orca/internal/wire"
	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The WorkloadService interface describes the workload operations the API exposes.
	WorkloadService interface {
		// Apply should store the given specification as desired state, reporting
		// whether the workload was newly created.
		Apply(ctx context.Context, spec manifest.Spec) (service.Workload, bool, error)
		// DryRun should report what applying the given specification would do,
		// without applying it.
		DryRun(ctx context.Context, spec manifest.Spec) (service.DryRun, error)
		// Get should return the workload with the given name.
		Get(ctx context.Context, name string) (service.Workload, error)
		// List should return the workloads matching every one of the given queries,
		// or all of them when none are given.
		List(ctx context.Context, queries ...string) ([]service.Workload, error)
		// Delete should mark the workload with the given name for deletion,
		// returning it as it now stands.
		Delete(ctx context.Context, name string, force bool) (service.Workload, error)
		// Stop should mark the workload with the given name as suspended,
		// returning it as it now stands.
		Stop(ctx context.Context, name string) (service.Workload, error)
		// Start should clear the suspension of the workload with the given name,
		// returning it as it now stands.
		Start(ctx context.Context, name string) (service.Workload, error)
		// Restart should ask for the workload's instances to be replaced,
		// returning the workload as it now stands.
		Restart(ctx context.Context, name string) (service.Workload, error)
		// Logs should write the recent output of the named workload to out, as the
		// options describe.
		Logs(ctx context.Context, out io.Writer, name string, options driver.LogOptions) error
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
	// The fewest log lines a request may ask for. The specification declares this
	// minimum, and it is enforced for a reason beyond tidiness: a driver may read a
	// non-positive count as meaning something other than a small tail. Docker takes a
	// negative one as "every line", which would return the whole of a workload's
	// output and make the maximum below bypassable by a minus sign.
	minLogTail = 1
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

// submitted returns the specification a request body describes, holding it to the
// name in the request path.
//
// The two endpoints taking a specification read it the same way, because a dry run
// that accepted a body the apply refuses would report on an apply that could not
// happen.
func (a *WorkloadAPI) submitted(body *api.WorkloadSpec, name string) (manifest.Spec, error) {
	if body == nil {
		return manifest.Spec{}, errors.New("request body is required")
	}

	// Converted to the canonical shape here, at the edge, so that everything below
	// this package reasons about a manifest.Spec rather than about the wire format.
	spec, err := wire.ToSpec(*body)
	if err != nil {
		return manifest.Spec{}, err
	}

	// The name appears in both the path and the body, so a mismatch is ambiguous
	// rather than something to silently resolve in favour of either one.
	if spec.Name != name {
		return manifest.Spec{}, fmt.Errorf("workload name %q does not match name %q in the request path", spec.Name, name)
	}

	return spec, nil
}

// ApplyWorkload stores the given specification as the desired state for the named
// workload.
func (a *WorkloadAPI) ApplyWorkload(ctx context.Context, request api.ApplyWorkloadRequestObject) (api.ApplyWorkloadResponseObject, error) {
	spec, err := a.submitted(request.Body, request.Name)
	if err != nil {
		return api.ApplyWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	workload, created, err := a.workloads.Apply(ctx, spec)
	switch {
	case errors.Is(err, service.ErrWorkloadDeleting):
		return api.ApplyWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is being deleted", request.Name),
		}, nil
	case errors.Is(err, port.ErrHostPortTaken):
		// A pinned host port another workload holds is a conflict with existing
		// state rather than a malformed request, and the message names the holder
		// so the fix is obvious.
		return api.ApplyWorkload409JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, port.ErrNoPortsAvailable):
		// The request is valid and will be servable once a port frees up, which is
		// what distinguishes this from a client error: nothing about the manifest
		// needs to change.
		return api.ApplyWorkload503JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrUnsupportedRuntime):
		return api.ApplyWorkload422JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrVolumeNotFound),
		errors.Is(err, service.ErrSecretNotFound),
		errors.Is(err, service.ErrVariableNotFound),
		errors.Is(err, service.ErrWorkloadNotFound),
		errors.Is(err, service.ErrPortNotPublished),
		errors.Is(err, service.ErrInvalidSpec),
		errors.Is(err, manifest.ErrNoRuntime),
		errors.Is(err, manifest.ErrAmbiguousRuntime):
		// Everything the specification names that does not exist is the caller's to
		// fix, and naming it is the whole point: the alternative is an operator who
		// mistyped one being told only that something went wrong.
		//
		// A workload that does not exist means one the specification references. This
		// endpoint creates the workload it is given, so it is never the workload being
		// applied that is missing.
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

// DryRunWorkload reports what applying the given specification would do, and
// applies nothing.
func (a *WorkloadAPI) DryRunWorkload(ctx context.Context, request api.DryRunWorkloadRequestObject) (api.DryRunWorkloadResponseObject, error) {
	spec, err := a.submitted(request.Body, request.Name)
	if err != nil {
		return api.DryRunWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	run, err := a.workloads.DryRun(ctx, spec)
	switch {
	case errors.Is(err, service.ErrWorkloadDeleting):
		return api.DryRunWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is being deleted", request.Name),
		}, nil
	case errors.Is(err, port.ErrHostPortTaken):
		return api.DryRunWorkload409JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrUnsupportedRuntime):
		return api.DryRunWorkload422JSONResponse{Error: err.Error()}, nil
	case errors.Is(err, service.ErrVolumeNotFound),
		errors.Is(err, service.ErrSecretNotFound),
		errors.Is(err, service.ErrVariableNotFound),
		errors.Is(err, service.ErrWorkloadNotFound),
		errors.Is(err, service.ErrPortNotPublished),
		errors.Is(err, service.ErrInvalidSpec),
		errors.Is(err, manifest.ErrNoRuntime),
		errors.Is(err, manifest.ErrAmbiguousRuntime):
		// Reported exactly as the apply reports it, so that a dry run which passes
		// is a statement about the apply rather than about the request.
		return api.DryRunWorkload400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	case err != nil:
		return api.DryRunWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("dry run workload", err),
			},
		}, nil
	}

	result := api.DryRunWorkload200JSONResponse{
		Spec:     wire.FromSpec(run.Spec),
		Created:  run.Created,
		Replaced: run.Replaced,
	}

	// Each is absent rather than empty when there is nothing to report. A hash of
	// "" would read as a hash, and a caller checking whether anything is unknown or
	// changed should not have to distinguish an empty list from a missing one.
	if run.SpecHash != "" {
		result.SpecHash = &run.SpecHash
	}

	if len(run.Unknown) > 0 {
		result.Unknown = &run.Unknown
	}

	if len(run.Changed) > 0 {
		result.Changed = &run.Changed
	}

	return result, nil
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
	force := request.Params.Force != nil && *request.Params.Force

	workload, err := a.workloads.Delete(ctx, request.Name, force)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.DeleteWorkload404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case errors.Is(err, service.ErrWorkloadInUse):
		// The workloads referencing it are named, because the caller's next question
		// is which ones, and answering it costs nothing here.
		return api.DeleteWorkload409JSONResponse{Error: err.Error()}, nil
	case err != nil:
		return api.DeleteWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("delete workload", err),
			},
		}, nil
	}

	return api.DeleteWorkload202JSONResponse{Workload: newWorkload(workload)}, nil
}

// StopWorkload marks the workload with the given name as suspended.
//
// The response is 202 for the same reason DeleteWorkload's is: the workload is
// only marked when the request returns, and the reconciler stops its work
// afterwards. A caller can watch the instances drain by polling the workload.
func (a *WorkloadAPI) StopWorkload(ctx context.Context, request api.StopWorkloadRequestObject) (api.StopWorkloadResponseObject, error) {
	workload, err := a.workloads.Stop(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.StopWorkload404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case errors.Is(err, service.ErrWorkloadDeleting):
		return api.StopWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is being deleted", request.Name),
		}, nil
	case err != nil:
		return api.StopWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("stop workload", err),
			},
		}, nil
	}

	return api.StopWorkload202JSONResponse{Workload: newWorkload(workload)}, nil
}

// StartWorkload clears the suspension of the workload with the given name.
//
// The response is 202 like StopWorkload's: the mark is cleared when the request
// returns, and the next reconcile pass starts the workload's instances.
func (a *WorkloadAPI) StartWorkload(ctx context.Context, request api.StartWorkloadRequestObject) (api.StartWorkloadResponseObject, error) {
	workload, err := a.workloads.Start(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.StartWorkload404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case errors.Is(err, service.ErrWorkloadDeleting):
		return api.StartWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is being deleted", request.Name),
		}, nil
	case err != nil:
		return api.StartWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("start workload", err),
			},
		}, nil
	}

	return api.StartWorkload202JSONResponse{Workload: newWorkload(workload)}, nil
}

// RestartWorkload asks for the named workload's instances to be replaced.
//
// The response is 202 like StopWorkload's: the request is only recorded when it
// returns, and the next reconcile pass performs the replacement.
func (a *WorkloadAPI) RestartWorkload(ctx context.Context, request api.RestartWorkloadRequestObject) (api.RestartWorkloadResponseObject, error) {
	workload, err := a.workloads.Restart(ctx, request.Name)
	switch {
	case errors.Is(err, service.ErrWorkloadNotFound):
		return api.RestartWorkload404JSONResponse{
			NotFoundJSONResponse: api.NotFoundJSONResponse{
				Error: fmt.Sprintf("workload %q does not exist", request.Name),
			},
		}, nil
	case errors.Is(err, service.ErrWorkloadDeleting):
		return api.RestartWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is being deleted", request.Name),
		}, nil
	case errors.Is(err, service.ErrWorkloadSuspended):
		return api.RestartWorkload409JSONResponse{
			Error: fmt.Sprintf("workload %q is suspended and cannot be restarted", request.Name),
		}, nil
	case err != nil:
		return api.RestartWorkload500JSONResponse{
			InternalServerErrorJSONResponse: api.InternalServerErrorJSONResponse{
				Error: a.internalError("restart workload", err),
			},
		}, nil
	}

	return api.RestartWorkload202JSONResponse{Workload: newWorkload(workload)}, nil
}

// GetWorkloadLogs returns the recent output of the named workload.
func (a *WorkloadAPI) GetWorkloadLogs(ctx context.Context, request api.GetWorkloadLogsRequestObject) (api.GetWorkloadLogsResponseObject, error) {
	// Clamped at both ends. The specification declares the range and the generated
	// code enforces neither, so the bounds the server documents are the server's to
	// apply.
	options := driver.LogOptions{Tail: defaultLogTail}
	if request.Params.Tail != nil {
		options.Tail = min(max(*request.Params.Tail, minLogTail), maxLogTail)
	}

	if request.Params.Previous != nil {
		options.Previous = *request.Params.Previous
	}

	if request.Params.Follow != nil {
		options.Follow = *request.Params.Follow
	}

	if request.Params.Since != nil {
		options.Since = *request.Params.Since
	}

	// A retained instance has already ended, so there is nothing for a follow of it to
	// wait on. Refused rather than answered as an ordinary read, because a caller who
	// asked to watch something should be told that it cannot be watched.
	if options.Follow && options.Previous {
		return api.GetWorkloadLogs400JSONResponse{
			BadRequestJSONResponse: api.BadRequestJSONResponse{
				Error: "cannot follow the previous instance, which has already ended",
			},
		}, nil
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
		follow: options.Follow,
		// The connection's own writer, which is the only one that can be given a
		// deadline. Nil when nothing put it there, and the read then lives under
		// whatever deadline the server set for every request.
		conn: Connection(ctx),
		write: func(w io.Writer) error {
			return a.workloads.Logs(ctx, w, request.Name, options)
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
	// Whether the response stays open for as long as the workload keeps writing.
	follow bool
	// The connection's own writer, for the deadline the wrappers cannot carry.
	conn  http.ResponseWriter
	write func(w io.Writer) error
}

// VisitGetWorkloadLogsResponse writes the logs to w as plain text.
//
// A followed read is exempt from the server's write timeout and is flushed as it goes.
// Both are needed for the same reason: the response is open for as long as the workload
// runs, which is longer than any deadline a request should have and longer than a
// caller can wait for a buffer to fill.
func (r logsResponse) VisitGetWorkloadLogsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/plain")

	out := io.Writer(w)

	if r.follow {
		// The deadline is set on the connection's own writer and the flushing is done
		// through the handler's. Only the first can carry a deadline, and only the
		// second counts what was written for the telemetry wrapped around it.
		deadline := r.conn
		if deadline == nil {
			deadline = w
		}

		// The zero time removes the deadline rather than extending it. A follow that
		// hit one would end as a truncated stream at exactly the timeout, which reads
		// as a workload that stopped talking rather than as a server that hung up.
		//
		// Nothing is left unbounded by this. The request's context ends the read when
		// the caller disconnects, which is what actually limits how long a stream
		// occupies the server.
		//
		// A writer with no deadline to clear says so, and there is nothing to do about
		// that but carry on. The stream then lives as long as that writer allows, which
		// is more than refusing to serve the request at all would give anybody.
		err := http.NewResponseController(deadline).SetWriteDeadline(time.Time{})
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			return fmt.Errorf("failed to clear the write deadline: %w", err)
		}

		out = &flushWriter{inner: w, control: http.NewResponseController(w)}
	}

	w.WriteHeader(http.StatusOK)

	return r.write(out)
}

// The flushWriter type pushes each write out to the client rather than letting it sit
// in a buffer.
//
// Without this a followed read arrives in chunks whenever the buffer happens to fill,
// which for a quiet workload may be a long time after the line was written. Watching a
// workload start is the whole point of following it, so a line held back is a line that
// did not arrive.
type flushWriter struct {
	inner   io.Writer
	control *http.ResponseController
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if err != nil {
		return n, err
	}

	// A response that cannot be flushed is still a response. The output reaches the
	// caller when the buffer fills, which is worse than immediately and better than
	// failing the read over it.
	_ = w.control.Flush()

	return n, nil
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

// newResolvedPorts maps the ports the server settled on onto the wire format.
func newResolvedPorts(ports []service.ResolvedPort) []api.ResolvedPort {
	resolved := make([]api.ResolvedPort, 0, len(ports))
	for _, port := range ports {
		entry := api.ResolvedPort{
			To:       port.To,
			From:     port.From,
			Protocol: api.Protocol(port.Protocol),
			Dynamic:  port.Dynamic,
		}

		// Reported as absent rather than as an empty string for a port the
		// specification did not name, which is how the field is sent everywhere else.
		if port.Name != "" {
			entry.Name = new(port.Name)
		}

		resolved = append(resolved, entry)
	}

	return resolved
}

// newWorkload maps the service's view of a workload onto the wire format.
func newWorkload(w service.Workload) api.Workload {
	workload := api.Workload{
		Name:      w.Name,
		Version:   w.Version,
		Runtime:   api.Runtime(w.Runtime),
		State:     api.WorkloadState(w.State),
		Spec:      wire.FromSpec(w.Spec),
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}

	if !w.NextRun.IsZero() {
		workload.NextRun = new(w.NextRun)
	}

	if w.LastError != "" {
		workload.LastError = new(w.LastError)
		workload.LastErrorAt = new(w.LastErrorAt)
	}

	if w.Deleting {
		workload.Deleting = new(true)
	}

	if w.Suspended {
		workload.Suspended = new(true)
	}

	if len(w.Ports) > 0 {
		workload.Ports = new(newResolvedPorts(w.Ports))
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
