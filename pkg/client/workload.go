package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/wire"
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
		// Whether the workload has been stopped and is intentionally not running.
		Suspended bool
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
		// Why the last attempt to converge the workload failed. Empty for one whose
		// last attempt succeeded, or that none has been made for.
		LastError string
		// When the last converge failure was recorded, meaningful only when
		// LastError is set.
		LastErrorAt time.Time
	}

	// The DryRun type reports what applying a specification would do, none of it
	// having been done.
	DryRun struct {
		// The specification as the server would store it: defaults in place,
		// volumes resolved to the paths they live at, and every port settled that
		// could be settled without allocating one.
		Spec manifest.Spec
		// The hash the apply would store. Empty when a host port has yet to be
		// allocated, since the allocation reaches the hash and nothing has chosen
		// one.
		SpecHash string
		// Whether nothing holds the name, so applying creates the workload.
		Created bool
		// Whether applying moves the stored hash, so the running instances are
		// replaced. False for a workload that does not exist.
		Replaced bool
		// The paths into Spec whose values the server settles only as it applies. A
		// host port it has yet to allocate is the only one.
		Unknown []string
		// The paths into Spec that differ from the specification the server stores.
		// Empty for a workload that does not exist, which has nothing to differ
		// from.
		//
		// A workload can be replaced with none of these set. The hash covers what a
		// workload reads as well as what it says, so an image rebuilt under the same
		// tag replaces an instance with the manifest untouched.
		Changed []string
	}

	// The ResolvedPort type is a port mapping as the server applied it.
	ResolvedPort struct {
		// The index of the workload instance the port reaches. Zero for a
		// workload running one instance.
		Instance int
		// What the specification called the port, which is how the rest of a
		// manifest refers to it. Empty for a port the specification did not name.
		Name string
		// The port the workload listens on inside its runtime.
		To int
		// The host port that reaches it.
		From int
		// The transport protocol the port is published on, which is either tcp or
		// udp. The two are separate address spaces, so it is part of the address
		// rather than a detail of it.
		Protocol string
		// Whether the host port was allocated by the server rather than pinned by
		// the specification.
		Dynamic bool
	}

	// The Instance type is the client-side view of one unit of work the server is
	// running for a workload.
	Instance struct {
		// The server's opaque handle for this instance.
		ID string
		// The index of the instance among the workload's instances, counted from
		// zero up to the specification's count.
		Index int
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
	// WorkloadStateSuspended indicates the workload was stopped by an operator and
	// stays down until it is started again. Unlike stopped, the server does not
	// intend to fix it.
	WorkloadStateSuspended WorkloadState = "suspended"
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
	resp, err := c.api.ApplyWorkloadWithResponse(ctx, spec.Name, wire.FromSpec(spec))
	if err != nil {
		return Workload{}, false, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON201 != nil:
		workload, err := newWorkload(resp.JSON201.Workload)

		return workload, true, err
	case resp.JSON200 != nil:
		workload, err := newWorkload(resp.JSON200.Workload)

		return workload, false, err
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

// DryRun reports what applying spec would do, without applying it.
//
// The server resolves the specification exactly as an apply resolves it, so
// everything an apply refuses this refuses too: a volume, secret, variable or
// workload the specification names and nothing holds, a pinned host port another
// workload has, a workload being torn down, and a specification that is not
// runnable. A dry run that returns is therefore a statement about the apply.
//
// Nothing is allocated, so a port mapping needing a host port comes back without
// one and its path is named in Unknown.
//
// The fields that differ from the stored specification are named in Changed. They
// say what about the workload would move, where the hash says only that something
// would.
func (c *Client) DryRun(ctx context.Context, spec manifest.Spec) (DryRun, error) {
	resp, err := c.api.DryRunWorkloadWithResponse(ctx, spec.Name, wire.FromSpec(spec))
	if err != nil {
		return DryRun{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newDryRun(*resp.JSON200)
	case resp.JSON400 != nil:
		return DryRun{}, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON409 != nil:
		return DryRun{}, newError(http.StatusConflict, resp.JSON409)
	case resp.JSON422 != nil:
		return DryRun{}, newError(http.StatusUnprocessableEntity, resp.JSON422)
	case resp.JSON500 != nil:
		return DryRun{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return DryRun{}, newError(resp.StatusCode(), nil)
	}
}

// Get returns the workload with the given name, returning ErrWorkloadNotFound when
// no such workload exists.
func (c *Client) Get(ctx context.Context, name string) (Workload, error) {
	resp, err := c.api.GetWorkloadWithResponse(ctx, name)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newWorkload(resp.JSON200.Workload)
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
		return nil, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		workloads := make([]Workload, 0, len(resp.JSON200.Workloads))
		for _, reported := range resp.JSON200.Workloads {
			workload, err := newWorkload(reported)
			if err != nil {
				return nil, err
			}

			workloads = append(workloads, workload)
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
	// The LifecycleOption type is a function that modifies how a workload
	// lifecycle operation is performed.
	LifecycleOption func(*lifecycleConfig)

	lifecycleConfig struct {
		wait     bool
		interval time.Duration
		force    bool
	}
)

func defaultLifecycleConfig() *lifecycleConfig {
	return &lifecycleConfig{
		interval: 500 * time.Millisecond,
	}
}

// WithWait modifies a lifecycle operation to block until the server has finished
// acting on it: a delete until the workload has disappeared, a stop until
// nothing is running for it, a start until something is, and a restart until a
// replacement instance has appeared.
//
// Waiting is polling, so the call returns once that has happened, the context is
// cancelled, or the server reports an error.
func WithWait() LifecycleOption {
	return func(c *lifecycleConfig) { c.wait = true }
}

// WithWaitInterval modifies how often a waiting operation polls the server, and
// implies WithWait.
func WithWaitInterval(interval time.Duration) LifecycleOption {
	return func(c *lifecycleConfig) {
		c.wait = true
		c.interval = interval
	}
}

// WithForceDeleteWorkload deletes a workload even though another workload references
// its address. Those workloads are redeployed and then report the reference they can
// no longer resolve, retrying until something holds the name again.
//
// Read only by Delete. The other lifecycle calls leave the workload in place, so
// there is nothing for them to force.
func WithForceDeleteWorkload() LifecycleOption {
	return func(c *lifecycleConfig) { c.force = true }
}

// Delete marks the workload with the given name for deletion and returns it as it
// stood when marked, returning ErrWorkloadNotFound when no such workload exists.
//
// Deletion is asynchronous: the returned workload is reported as terminating, and
// disappears once the server has stopped everything running for it. Pass WithWait
// to block until that has happened.
//
// A workload another one references is refused with ErrWorkloadInUse, and the error
// names the workloads reading its address. Pass WithForceDeleteWorkload to delete it
// anyway.
func (c *Client) Delete(ctx context.Context, name string, options ...LifecycleOption) (Workload, error) {
	config := defaultLifecycleConfig()
	for _, option := range options {
		option(config)
	}

	params := api.DeleteWorkloadParams{}
	if config.force {
		params.Force = &config.force
	}

	resp, err := c.api.DeleteWorkloadWithResponse(ctx, name, &params)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to send the request: %w", err)
	}

	var workload Workload
	switch {
	case resp.JSON202 != nil:
		if workload, err = newWorkload(resp.JSON202.Workload); err != nil {
			return Workload{}, err
		}
	case resp.JSON404 != nil:
		return Workload{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON409 != nil:
		// The server's message stands on its own and already says the workload is in
		// use, naming what references it. The sentinel is joined to it rather than
		// prefixed onto it, so the reason is not stated twice.
		return Workload{}, fmt.Errorf("%s: %w", resp.JSON409.Error, ErrWorkloadInUse)
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

// Stop marks the workload with the given name as suspended and returns it as it
// stood when marked. Returns ErrWorkloadNotFound when no such workload exists,
// and IsConflict reports true of the error for a workload that is being deleted.
//
// Stopping is asynchronous: the mark holds the workload down and the server
// stops its instances afterwards. Pass WithWait to block until nothing is
// running for it. The specification and its version are untouched, so Start
// resumes the workload rather than replacing it. Suspension survives a server
// restart and holds until Start clears it.
func (c *Client) Stop(ctx context.Context, name string, options ...LifecycleOption) (Workload, error) {
	config := defaultLifecycleConfig()
	for _, option := range options {
		option(config)
	}

	resp, err := c.api.StopWorkloadWithResponse(ctx, name, api.StopWorkloadJSONRequestBody{})
	if err != nil {
		return Workload{}, fmt.Errorf("failed to send the request: %w", err)
	}

	var workload Workload
	switch {
	case resp.JSON202 != nil:
		if workload, err = newWorkload(resp.JSON202.Workload); err != nil {
			return Workload{}, err
		}
	case resp.JSON404 != nil:
		return Workload{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON409 != nil:
		return Workload{}, newError(http.StatusConflict, resp.JSON409)
	case resp.JSON500 != nil:
		return Workload{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Workload{}, newError(resp.StatusCode(), nil)
	}

	if !config.wait {
		return workload, nil
	}

	// The instance a stop leaves behind is still reported — it is what keeps the
	// workload's last output readable — so "nothing is running" means no instance
	// still up, not no instances at all.
	return c.waitFor(ctx, name, config.interval, func(w Workload) bool {
		return w.Suspended && !slices.ContainsFunc(w.Instances, instanceUp)
	})
}

// instanceUp reports whether an instance is still up or on its way in or out, as
// opposed to having ended.
func instanceUp(instance Instance) bool {
	switch instance.State {
	case InstanceStatePending, InstanceStateRunning, InstanceStateTerminating:
		return true
	default:
		return false
	}
}

// Start clears the suspension of the workload with the given name and returns it
// as it stood when cleared. Returns ErrWorkloadNotFound when no such workload
// exists, and IsConflict reports true of the error for a workload that is being
// deleted.
//
// Starting is asynchronous: the server starts the workload's instances on its
// next pass. Pass WithWait to block until something is up for the workload that
// was not up before — for a scheduled workload that is its next occurrence, so
// waiting on one blocks until the schedule next fires. Starting a workload that is
// not suspended changes nothing, and waiting on one returns as soon as it is read.
func (c *Client) Start(ctx context.Context, name string, options ...LifecycleOption) (Workload, error) {
	config := defaultLifecycleConfig()
	for _, option := range options {
		option(config)
	}

	// The instances are read before the request, for the same reason a restart reads
	// them. Stopping a workload keeps its last container so the output stays
	// readable, and that container is still reported once the suspension clears. A
	// wait that only asked whether the workload had left pending would be satisfied
	// by it and return before anything had started.
	var before map[string]struct{}
	if config.wait {
		current, err := c.Get(ctx, name)
		if err != nil {
			return Workload{}, err
		}

		before = make(map[string]struct{}, len(current.Instances))
		for _, instance := range current.Instances {
			before[instance.ID] = struct{}{}
		}
	}

	resp, err := c.api.StartWorkloadWithResponse(ctx, name, api.StartWorkloadJSONRequestBody{})
	if err != nil {
		return Workload{}, fmt.Errorf("failed to send the request: %w", err)
	}

	var workload Workload
	switch {
	case resp.JSON202 != nil:
		if workload, err = newWorkload(resp.JSON202.Workload); err != nil {
			return Workload{}, err
		}
	case resp.JSON404 != nil:
		return Workload{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON409 != nil:
		return Workload{}, newError(http.StatusConflict, resp.JSON409)
	case resp.JSON500 != nil:
		return Workload{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Workload{}, newError(resp.StatusCode(), nil)
	}

	if !config.wait {
		return workload, nil
	}

	return c.waitFor(ctx, name, config.interval, func(w Workload) bool {
		if w.Suspended {
			return false
		}

		// An instance that was not there before is the start having happened, once
		// it has left pending. A container is reported from the moment it is
		// created, so a wait satisfied by its existence alone races its startup
		// and returns a workload that is not yet up. One that was there and is
		// running covers starting a workload that was never suspended, where
		// nothing new appears because nothing had to.
		return slices.ContainsFunc(w.Instances, func(instance Instance) bool {
			if _, existed := before[instance.ID]; !existed {
				return instance.State != InstanceStatePending
			}

			return instance.State == InstanceStateRunning
		})
	})
}

// Restart asks the server to replace the workload's running instances and
// returns the workload as it stood when the request was recorded. Returns
// ErrWorkloadNotFound when no such workload exists, and IsConflict reports true
// of the error for a workload that is being deleted or is suspended.
//
// The replacement happens on the server's next pass, from the unchanged
// specification, so the version does not move. Pass WithWait to block until an
// instance that did not exist before the request has started.
func (c *Client) Restart(ctx context.Context, name string, options ...LifecycleOption) (Workload, error) {
	config := defaultLifecycleConfig()
	for _, option := range options {
		option(config)
	}

	// The instances are read before the request, because "the restart happened" is
	// only observable as an instance that was not there before it.
	var before map[string]struct{}
	if config.wait {
		current, err := c.Get(ctx, name)
		if err != nil {
			return Workload{}, err
		}

		before = make(map[string]struct{}, len(current.Instances))
		for _, instance := range current.Instances {
			before[instance.ID] = struct{}{}
		}
	}

	resp, err := c.api.RestartWorkloadWithResponse(ctx, name, api.RestartWorkloadJSONRequestBody{})
	if err != nil {
		return Workload{}, fmt.Errorf("failed to send the request: %w", err)
	}

	var workload Workload
	switch {
	case resp.JSON202 != nil:
		if workload, err = newWorkload(resp.JSON202.Workload); err != nil {
			return Workload{}, err
		}
	case resp.JSON404 != nil:
		return Workload{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, resp.JSON404.Error)
	case resp.JSON409 != nil:
		return Workload{}, newError(http.StatusConflict, resp.JSON409)
	case resp.JSON500 != nil:
		return Workload{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Workload{}, newError(resp.StatusCode(), nil)
	}

	if !config.wait {
		return workload, nil
	}

	return c.waitFor(ctx, name, config.interval, func(w Workload) bool {
		// The replacement counts once it has left pending, for the reason the
		// start wait gives: a container is reported from the moment it is
		// created, and one still starting is not yet the replacement being up.
		return slices.ContainsFunc(w.Instances, func(instance Instance) bool {
			_, existed := before[instance.ID]

			return !existed && instance.State != InstanceStatePending
		})
	})
}

// waitFor polls the workload until it satisfies the given condition, returning it
// as it then stands.
func (c *Client) waitFor(ctx context.Context, name string, interval time.Duration, settled func(Workload) bool) (Workload, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Workload{}, ctx.Err()
		case <-ticker.C:
			workload, err := c.Get(ctx, name)
			if err != nil {
				return Workload{}, fmt.Errorf("failed to wait for workload: %w", err)
			}

			if settled(workload) {
				return workload, nil
			}
		}
	}
}

type (
	// The LogOption type is a function that modifies which output is read.
	LogOption func(*logConfig)

	logConfig struct {
		tail     int
		previous bool
		follow   bool
		since    time.Time
		instance *int
	}
)

// WithTail modifies a read to return only the last lines of the output.
//
// A count of zero or less leaves the limit to the server, which is also what happens
// when this is not passed at all. The server caps what it will read either way, so a
// generous count is answered with as much as it is willing to serve rather than
// refused.
func WithTail(lines int) LogOption {
	return func(c *logConfig) { c.tail = lines }
}

// WithPrevious modifies a read to return the output of the instance that was replaced
// rather than the one running now.
//
// The server keeps the instance it most recently stopped so that the output of an
// attempt that ended survives it. For a workload restarting repeatedly this is the
// attempt that failed, where the one running now has not failed yet. A workload that
// has only ever run once has no earlier attempt, and nothing is written.
func WithPrevious() LogOption {
	return func(c *logConfig) { c.previous = true }
}

// WithFollow modifies a read to keep writing output as the workload produces it.
//
// The read ends when the instance ends, which is what somebody watching a workload
// start is waiting for either way. A replacement is a new instance, so a workload that
// restarts while you watch needs another read.
//
// Cancelling the context is how a caller ends a follow early, and it ends the read
// rather than failing it. This cannot be combined with WithPrevious, which reads an
// instance that has already ended.
func WithFollow() LogOption {
	return func(c *logConfig) { c.follow = true }
}

// WithInstance modifies a read to return one instance's output, selected by its
// index.
//
// Without it every instance's output is returned, except on a follow of a workload
// running more than one instance, which the server refuses: a follow reads one
// stream until it ends, and interleaving several would return output nothing could
// attribute.
func WithInstance(index int) LogOption {
	return func(c *logConfig) { c.instance = new(index) }
}

// WithSince modifies a read to return only the output written at or after an instant.
//
// This reaches container workloads only. An exec workload's output is a plain file with
// no timestamps in it, so the server ignores this rather than filtering on times it
// would have to invent.
func WithSince(t time.Time) LogOption {
	return func(c *logConfig) { c.since = t }
}

// Logs writes the recent output of the named workload to out, as the options describe.
//
// Passing no options reads the current instance with the server deciding how much of it
// to return.
//
// A followed read does not return until the instance ends or the context is cancelled.
// Cancellation is not reported as a failure: it is how a caller ends a follow.
//
// The output is copied as it arrives rather than returned, so a workload with a lot
// of output doesn't have to fit in the caller's memory before any of it is usable.
func (c *Client) Logs(ctx context.Context, out io.Writer, name string, options ...LogOption) error {
	var config logConfig
	for _, option := range options {
		option(&config)
	}

	var params api.GetWorkloadLogsParams
	if config.tail > 0 {
		params.Tail = new(config.tail)
	}

	if config.previous {
		params.Previous = new(true)
	}

	if config.follow {
		params.Follow = new(true)
	}

	if !config.since.IsZero() {
		params.Since = &config.since
	}

	params.Instance = config.instance

	// A follow is the one request that legitimately outlives the client's timeout, so
	// it goes out over the client that has none. What ends it is the caller's context.
	inner := c.api
	if config.follow {
		inner = c.stream
	}

	resp, err := inner.GetWorkloadLogs(ctx, name, &params)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.logsError(resp)
	}

	if _, err = io.Copy(out, resp.Body); err != nil {
		// A caller who cancelled a follow already knows why the output stopped, and
		// the copy fails in whatever way the transport noticed first. Reporting that
		// would turn an ordinary Ctrl-C into an error.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("failed to read the response body: %w", err)
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

// newDryRun converts what the server reported about an apply into the client's own
// shape.
func newDryRun(result api.DryRunWorkloadResult) (DryRun, error) {
	spec, err := wire.ToSpec(result.Spec)
	if err != nil {
		return DryRun{}, fmt.Errorf("failed to read the reported specification: %w", err)
	}

	run := DryRun{
		Spec:     spec,
		Created:  result.Created,
		Replaced: result.Replaced,
	}

	if result.SpecHash != nil {
		run.SpecHash = *result.SpecHash
	}

	if result.Unknown != nil {
		run.Unknown = *result.Unknown
	}

	if result.Changed != nil {
		run.Changed = *result.Changed
	}

	return run, nil
}

// newWorkload maps a workload the server reported onto the shape the client returns.
//
// It fails when the specification does not convert, which means the server sent one
// this client cannot read. That is a malformed response rather than something the
// caller did, so it is reported rather than silently returning a workload missing
// whatever did not convert.
func newWorkload(w api.Workload) (Workload, error) {
	spec, err := wire.ToSpec(w.Spec)
	if err != nil {
		return Workload{}, fmt.Errorf("failed to read the specification of workload %s: %w", w.Name, err)
	}

	workload := Workload{
		Name:      w.Name,
		Version:   w.Version,
		Runtime:   manifest.Runtime(w.Runtime),
		State:     WorkloadState(w.State),
		Spec:      spec,
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}

	if w.NextRun != nil {
		workload.NextRun = *w.NextRun
	}
	if w.Deleting != nil {
		workload.Deleting = *w.Deleting
	}
	if w.Suspended != nil {
		workload.Suspended = *w.Suspended
	}
	if w.LastError != nil {
		workload.LastError = *w.LastError
	}
	if w.LastErrorAt != nil {
		workload.LastErrorAt = *w.LastErrorAt
	}

	if w.Ports != nil {
		workload.Ports = make([]ResolvedPort, 0, len(*w.Ports))
		for _, port := range *w.Ports {
			resolved := ResolvedPort{
				To:       port.To,
				From:     port.From,
				Protocol: string(port.Protocol),
				Dynamic:  port.Dynamic,
			}

			if port.Instance != nil {
				resolved.Instance = *port.Instance
			}

			if port.Name != nil {
				resolved.Name = *port.Name
			}

			workload.Ports = append(workload.Ports, resolved)
		}
	}

	if w.Instances == nil {
		return workload, nil
	}

	workload.Instances = make([]Instance, 0, len(*w.Instances))
	for _, instance := range *w.Instances {
		mapped := Instance{
			ID:       instance.ID,
			State:    InstanceState(instance.State),
			SpecHash: instance.SpecHash,
			ExitCode: instance.ExitCode,
		}

		if instance.Index != nil {
			mapped.Index = *instance.Index
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

	return workload, nil
}
