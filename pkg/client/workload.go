package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The Workload type is the client-side view of a workload: the desired state
	// that was submitted, together with what the server reports is running for it.
	Workload struct {
		// The name that identifies the workload.
		Name string
		// Incremented every time the workload's specification changes.
		Version int
		// Which runtime the specification names.
		Runtime string
		// The workload's overall state, derived from its instances.
		State string
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
		State string
		// The hash of the specification the instance was started from.
		SpecHash string
		// The exit code. Nil while the instance is still running.
		ExitCode *int
		// When the instance last started, if it has started at all.
		StartedAt time.Time
	}
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

// Logs returns the recent output of the named workload, limited to the last tail
// lines. A tail of zero leaves the limit to the server.
func (c *Client) Logs(ctx context.Context, name string, tail int) (string, error) {
	var params api.GetWorkloadLogsParams
	if tail > 0 {
		params.Tail = new(tail)
	}

	resp, err := c.api.GetWorkloadLogsWithResponse(ctx, name, &params)
	if err != nil {
		return "", fmt.Errorf("failed to read workload logs: %w", err)
	}

	switch {
	case resp.StatusCode() == http.StatusOK:
		return string(resp.Body), nil
	case resp.JSON404 != nil:
		return "", fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON500 != nil:
		return "", newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return "", newError(resp.StatusCode(), nil)
	}
}

func newWorkload(w api.Workload) Workload {
	workload := Workload{
		Name:      w.Name,
		Version:   w.Version,
		Runtime:   string(w.Runtime),
		State:     string(w.State),
		Spec:      newSpec(w.Spec),
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
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
			State:    string(instance.State),
			SpecHash: instance.SpecHash,
			ExitCode: instance.ExitCode,
		}

		if instance.StartedAt != nil {
			mapped.StartedAt = *instance.StartedAt
		}

		workload.Instances = append(workload.Instances, mapped)
	}

	return workload
}
