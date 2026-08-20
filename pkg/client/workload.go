package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The WorkloadState type describes the overall state of a workload, derived by
	// the server from the instances it is running.
	WorkloadState string

	// The InstanceState type describes the state of a single unit of work the server
	// is running for a workload.
	//
	// It is distinct from WorkloadState because the two do not share a vocabulary: an
	// instance exits, where a workload whose instances have all exited is stopped.
	InstanceState string

	// The HealthStatus type describes whether a workload is working, as distinct from
	// whether its runtime reports it started.
	HealthStatus string

	// The Workload type is the client-side view of a workload: the desired state
	// that was submitted, together with what the server reports is running for it.
	Workload struct {
		// The name that identifies the workload.
		Name string
		// Incremented every time the workload's specification changes.
		Version int
		// Which runtime the specification names.
		Runtime manifest.Runtime
		// The workload's overall state, derived from its instances.
		State WorkloadState
		// Whether the workload is being torn down and will shortly disappear.
		Deleting bool
		// The specification that was submitted.
		Spec manifest.Spec
		// The port mappings the server settled on, including any it allocated. These
		// are how the workload is reached.
		Ports []ResolvedPort
		// The instances the server is currently running for the workload.
		Instances []Instance
		// The time the workload was first applied.
		CreatedAt time.Time
		// The time the workload's specification last changed.
		UpdatedAt time.Time
		// When the workload next runs, for one that names a schedule. Zero for a
		// workload that runs continuously.
		NextRun time.Time
	}

	// The ResolvedPort type is a port mapping as the server applied it.
	ResolvedPort struct {
		// The port the workload listens on inside its runtime.
		To int
		// The host port that reaches it.
		From int
		// Whether the host port was allocated by the server rather than pinned by
		// the specification.
		Dynamic bool
	}

	// The Instance type is the client-side view of one unit of work the server is
	// running for a workload.
	Instance struct {
		// The server's opaque handle for this instance.
		ID string
		// The instance's current state.
		State InstanceState
		// The hash of the specification the instance was started from.
		SpecHash string
		// The exit code. Nil while the instance is still running.
		ExitCode *int
		// When the instance last started, if it has started at all.
		StartedAt time.Time
		// What the most recent health check established. Nil when the instance is
		// not checked at all.
		Health *Health
	}

	// The Health type is the client-side view of a health check's outcome.
	Health struct {
		// Whether the instance is working.
		Status HealthStatus
		// How many consecutive checks have failed. Nil when the check is the
		// runtime's own rather than one orca performs, since orca counts no
		// failures against a check it did not run.
		Failures *int
		// When the check last ran, if it has run at all.
		CheckedAt time.Time
		// Why the last check failed, when it did.
		Error string
	}
)

const (
	// WorkloadStatePending indicates nothing is running for the workload yet, either
	// because it has just been applied or because it is waiting to be restarted.
	WorkloadStatePending WorkloadState = "pending"
	// WorkloadStateRunning indicates the workload's instances are up.
	WorkloadStateRunning WorkloadState = "running"
	// WorkloadStateTerminating indicates the workload is being torn down and will
	// shortly disappear.
	WorkloadStateTerminating WorkloadState = "terminating"
	// WorkloadStateStopped indicates the workload's instances have all ended without
	// failing, and the server intends to restart them.
	WorkloadStateStopped WorkloadState = "stopped"
	// WorkloadStateCompleted indicates the workload ended cleanly and its restart
	// policy asks for nothing further. Unlike stopped, this is the desired end state.
	WorkloadStateCompleted WorkloadState = "completed"
	// WorkloadStateFailed indicates the workload is not working, whether because an
	// instance failed or because it is not passing its health check.
	WorkloadStateFailed WorkloadState = "failed"
)

const (
	// InstanceStatePending indicates the instance has been created but is not yet
	// running.
	InstanceStatePending InstanceState = "pending"
	// InstanceStateRunning indicates the instance is up.
	InstanceStateRunning InstanceState = "running"
	// InstanceStateTerminating indicates the instance is on its way out.
	InstanceStateTerminating InstanceState = "terminating"
	// InstanceStateExited indicates the instance ended without failing.
	InstanceStateExited InstanceState = "exited"
	// InstanceStateCompleted indicates the instance ended cleanly and its workload's
	// restart policy asks for nothing further.
	InstanceStateCompleted InstanceState = "completed"
	// InstanceStateFailed indicates the instance ended in failure.
	InstanceStateFailed InstanceState = "failed"
)

const (
	// HealthStarting indicates the workload has not yet passed a check, either
	// because it is inside its start period or because nothing has answered yet.
	HealthStarting HealthStatus = "starting"
	// HealthHealthy indicates the workload's most recent check passed.
	HealthHealthy HealthStatus = "healthy"
	// HealthUnhealthy indicates enough consecutive checks have failed to exhaust the
	// workload's retries.
	HealthUnhealthy HealthStatus = "unhealthy"
)

// Apply stores spec as the desired state for its name, reporting whether the
// workload was newly created.
//
// Applying an unchanged specification is a no-op that leaves the workload's
// version alone.
func (c *Client) Apply(ctx context.Context, spec manifest.Spec) (Workload, bool, error) {
	resp, err := c.api.ApplyWorkloadWithResponse(ctx, spec.Name, wireSpec(spec))
	if err != nil {
		return Workload{}, false, fmt.Errorf("failed to apply workload: %w", err)
	}

	switch {
	case resp.JSON201 != nil:
		return newWorkload(*resp.JSON201), true, nil
	case resp.JSON200 != nil:
		return newWorkload(*resp.JSON200), false, nil
	case resp.JSON400 != nil:
		return Workload{}, false, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON409 != nil:
		return Workload{}, false, newError(http.StatusConflict, resp.JSON409)
	case resp.JSON422 != nil:
		return Workload{}, false, newError(http.StatusUnprocessableEntity, resp.JSON422)
	case resp.JSON503 != nil:
		return Workload{}, false, newError(http.StatusServiceUnavailable, resp.JSON503)
	case resp.JSON500 != nil:
		return Workload{}, false, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Workload{}, false, newError(resp.StatusCode(), nil)
	}
}

// Get returns the workload with the given name, returning ErrWorkloadNotFound when
// no such workload exists.
func (c *Client) Get(ctx context.Context, name string) (Workload, error) {
	resp, err := c.api.GetWorkloadWithResponse(ctx, name)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to get workload: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newWorkload(*resp.JSON200), nil
	case resp.JSON404 != nil:
		return Workload{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON500 != nil:
		return Workload{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Workload{}, newError(resp.StatusCode(), nil)
	}
}

// List returns the workloads matching every one of the given queries, or all of them
// when none are given.
//
// A query is a "path=value" filter over the workload's specification, where the path
// is a JSON path such as "$.labels.app". Values are compared as text, so a number is
// matched by its digits.
func (c *Client) List(ctx context.Context, queries ...string) ([]Workload, error) {
	var params api.ListWorkloadsParams
	if len(queries) > 0 {
		params.Query = &queries
	}

	resp, err := c.api.ListWorkloadsWithResponse(ctx, &params)
	if err != nil {
		return nil, fmt.Errorf("failed to list workloads: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		workloads := make([]Workload, 0, len(*resp.JSON200))
		for _, workload := range *resp.JSON200 {
			workloads = append(workloads, newWorkload(workload))
		}

		return workloads, nil
	case resp.JSON400 != nil:
		return nil, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return nil, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return nil, newError(resp.StatusCode(), nil)
	}
}

type (
	// The DeleteOption type is a function that modifies how a delete is performed.
	DeleteOption func(*deleteConfig)

	deleteConfig struct {
		wait     bool
		interval time.Duration
	}
)

func defaultDeleteConfig() *deleteConfig {
	return &deleteConfig{
		interval: 500 * time.Millisecond,
	}
}

// WithWait modifies a delete to block until the server has finished tearing the
// workload down and it has disappeared, rather than returning as soon as it has
// been marked for deletion.
//
// Waiting is polling, so the call returns once the workload is gone, the context is
// cancelled, or the server reports an error.
func WithWait() DeleteOption {
	return func(c *deleteConfig) { c.wait = true }
}

// WithWaitInterval modifies how often a waiting delete polls the server, and
// implies WithWait.
func WithWaitInterval(interval time.Duration) DeleteOption {
	return func(c *deleteConfig) {
		c.wait = true
		c.interval = interval
	}
}

// Delete marks the workload with the given name for deletion and returns it as it
// stood when marked, returning ErrWorkloadNotFound when no such workload exists.
//
// Deletion is asynchronous: the returned workload is reported as terminating, and
// disappears once the server has stopped everything running for it. Pass WithWait
// to block until that has happened.
func (c *Client) Delete(ctx context.Context, name string, options ...DeleteOption) (Workload, error) {
	config := defaultDeleteConfig()
	for _, option := range options {
		option(config)
	}

	resp, err := c.api.DeleteWorkloadWithResponse(ctx, name)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to delete workload: %w", err)
	}

	var workload Workload
	switch {
	case resp.JSON202 != nil:
		workload = newWorkload(*resp.JSON202)
	case resp.JSON404 != nil:
		return Workload{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON500 != nil:
		return Workload{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Workload{}, newError(resp.StatusCode(), nil)
	}

	if !config.wait {
		return workload, nil
	}

	// The workload returned is the one that was marked, not one re-read after the
	// teardown: by the time waiting finishes there is nothing left to read.
	if err = c.waitForTeardown(ctx, name, config.interval); err != nil {
		return Workload{}, err
	}

	return workload, nil
}

// waitForTeardown polls the workload until the server reports it is gone, which is
// how a caller observes an asynchronous deletion completing.
func (c *Client) waitForTeardown(ctx context.Context, name string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, err := c.Get(ctx, name)
			switch {
			case errors.Is(err, ErrWorkloadNotFound):
				return nil
			case err != nil:
				return fmt.Errorf("failed to wait for workload deletion: %w", err)
			}
		}
	}
}

// Logs writes the recent output of the named workload to out, limited to the last
// tail lines. A tail of zero leaves the limit to the server.
//
// The output is copied as it arrives rather than returned, so a workload with a lot
// of output doesn't have to fit in the caller's memory before any of it is usable.
func (c *Client) Logs(ctx context.Context, out io.Writer, name string, tail int) error {
	var params api.GetWorkloadLogsParams
	if tail > 0 {
		params.Tail = new(tail)
	}

	resp, err := c.api.GetWorkloadLogs(ctx, name, &params)
	if err != nil {
		return fmt.Errorf("failed to read workload logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.logsError(resp)
	}

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to read workload logs: %w", err)
	}

	return nil
}

// logsError turns an unsuccessful logs response into an error, decoding the server's
// message where it sent one.
//
// The body is read under a limit because this is the one response the client decodes
// itself: an error message is a sentence, and something answering this endpoint with
// an endless one should cost the caller a failed request rather than its memory.
func (c *Client) logsError(resp *http.Response) error {
	var body api.ErrorResponse
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrWorkloadNotFound, body.Error)
	}

	return newError(resp.StatusCode, &body)
}

func newWorkload(w api.Workload) Workload {
	workload := Workload{
		Name:      w.Name,
		Version:   w.Version,
		Runtime:   manifest.Runtime(w.Runtime),
		State:     WorkloadState(w.State),
		Spec:      manifest.NewSpec(w.Spec),
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}

	if w.NextRun != nil {
		workload.NextRun = *w.NextRun
	}
	if w.Deleting != nil {
		workload.Deleting = *w.Deleting
	}

	if w.Ports != nil {
		workload.Ports = make([]ResolvedPort, 0, len(*w.Ports))
		for _, port := range *w.Ports {
			workload.Ports = append(workload.Ports, ResolvedPort{
				To:      port.To,
				From:    port.From,
				Dynamic: port.Dynamic,
			})
		}
	}

	if w.Instances == nil {
		return workload
	}

	workload.Instances = make([]Instance, 0, len(*w.Instances))
	for _, instance := range *w.Instances {
		mapped := Instance{
			ID:       instance.ID,
			State:    InstanceState(instance.State),
			SpecHash: instance.SpecHash,
			ExitCode: instance.ExitCode,
		}

		if instance.StartedAt != nil {
			mapped.StartedAt = *instance.StartedAt
		}

		if instance.Health != nil {
			mapped.Health = &Health{
				Status:   HealthStatus(instance.Health.Status),
				Failures: instance.Health.Failures,
			}

			if instance.Health.CheckedAt != nil {
				mapped.Health.CheckedAt = *instance.Health.CheckedAt
			}
			if instance.Health.Error != nil {
				mapped.Health.Error = *instance.Health.Error
			}
		}

		workload.Instances = append(workload.Instances, mapped)
	}

	return workload
}
