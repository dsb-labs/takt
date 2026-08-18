// Package docker provides the driver that runs orca workloads as Docker containers.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
)

const (
	// LabelWorkload names the container label holding the workload a container
	// belongs to. It is how the driver recognises its own work.
	LabelWorkload = "orca.workload"
	// LabelSpecHash names the container label holding the hash of the
	// specification a container was created from.
	LabelSpecHash = "orca.spec-hash"
	// LabelVersion names the container label holding the workload version a
	// container was created from.
	LabelVersion = "orca.version"
)

var (
	// ErrInvalidPorts is returned when a workload's port mappings cannot be parsed.
	ErrInvalidPorts = errors.New("invalid port mapping")
	// ErrNotContainerWorkload is returned when NewWorkload is given a stored
	// workload whose specification carries no container block.
	ErrNotContainerWorkload = errors.New("workload does not describe a container")
)

type (
	// The Driver type runs workloads as Docker containers.
	Driver struct {
		logger *slog.Logger
		client Client
	}

	// The Config type contains fields used to construct a Driver.
	Config struct {
		// The logger used for driver lifecycle events.
		Logger *slog.Logger
		// The client used to talk to the Docker daemon.
		Client Client
	}

	// The Workload type describes the container a driver should run for a workload.
	//
	// It is the driver's own view of desired state, deliberately independent of
	// both the wire format and the database schema so that neither has to change
	// shape when a driver gains a capability.
	Workload struct {
		// The name of the workload the container belongs to.
		Name string
		// The version of the specification the container is created from.
		Version int
		// The hash of the specification the container is created from, recorded on
		// the container so that drift can be detected later.
		SpecHash string
		// The image the container runs.
		Image string
		// The environment variables set inside the container.
		Env map[string]string
		// The port mappings to publish, in Docker's "host:container" form.
		Ports []string
		// Arbitrary key-value pairs to attach to the container alongside orca's
		// own labels.
		Labels map[string]string
	}
)

// New returns a Driver that runs containers through the client in config.
func New(config Config) *Driver {
	return &Driver{
		logger: config.Logger.With("component", "driver", "driver", "docker"),
		client: config.Client,
	}
}

// NewWorkload maps a stored workload onto the driver's own view of it, decoding
// the container block from the specification the server persisted.
//
// Returns ErrNotContainerWorkload when the specification carries no container
// block, which means it was meant for a different driver.
func NewWorkload(row database.Workload) (Workload, error) {
	var spec api.WorkloadSpec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return Workload{}, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	if spec.Container == nil {
		return Workload{}, ErrNotContainerWorkload
	}

	w := Workload{
		Name:     row.Name,
		Version:  row.Version,
		SpecHash: row.SpecHash,
		Image:    spec.Container.Image,
		Labels:   row.Labels,
	}

	if spec.Container.Env != nil {
		w.Env = *spec.Container.Env
	}
	if spec.Container.Ports != nil {
		w.Ports = *spec.Container.Ports
	}

	return w, nil
}

// Start creates and starts a container for the given workload, pulling its image
// first when it isn't already present locally.
//
// The container is created with orca's ownership labels, which is the only record
// that ties it back to a workload — the server stores no container identifier.
// Docker's own restart policy is deliberately left unset: orca owns restart
// decisions so that they can be paced by its own backoff and remain visible in
// the workload's observed state.
func (d *Driver) Start(ctx context.Context, w Workload) (string, error) {
	if err := d.ensureImage(ctx, w.Image); err != nil {
		return "", err
	}

	exposed, bindings, err := nat.ParsePortSpecs(w.Ports)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPorts, err)
	}

	labels := make(map[string]string, len(w.Labels)+3)
	for k, v := range w.Labels {
		labels[k] = v
	}
	labels[LabelWorkload] = w.Name
	labels[LabelSpecHash] = w.SpecHash
	labels[LabelVersion] = strconv.Itoa(w.Version)

	created, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image:        w.Image,
			Env:          environment(w.Env),
			Labels:       labels,
			ExposedPorts: exposed,
		},
		&container.HostConfig{
			PortBindings: bindings,
		},
		nil, nil,
		containerName(w.Name, w.Version),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	if err = d.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// The container exists but won't run. Remove it so the next reconcile
		// starts from a clean slate rather than finding a created-but-dead
		// container it would have to reason about.
		if removeErr := d.client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true}); removeErr != nil {
			d.logger.With("workload", w.Name, "error", removeErr).Error("failed to remove container after failed start")
		}

		return "", fmt.Errorf("failed to start container: %w", err)
	}

	d.logger.With("workload", w.Name, "container", created.ID, "version", w.Version).Debug("container started")

	return created.ID, nil
}

// Stop stops and removes every container the driver holds for the named workload.
//
// Removal is forced because ContainerStop only asks the container to stop and
// returns before it necessarily has: an unforced remove races that shutdown and
// fails with "container is running", which would leave the container behind for
// every future pass to trip over. The stop is still issued first so the container
// gets its grace period rather than being killed outright.
func (d *Driver) Stop(ctx context.Context, workload string) error {
	containers, err := d.containers(ctx, workload)
	if err != nil {
		return err
	}

	for _, c := range containers {
		if err = d.client.ContainerStop(ctx, c.ID, container.StopOptions{}); err != nil {
			return fmt.Errorf("failed to stop container: %w", err)
		}

		if err = d.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("failed to remove container: %w", err)
		}

		d.logger.With("workload", workload, "container", c.ID).Debug("container stopped")
	}

	return nil
}

// Observe returns an instance for every container the driver owns, whatever state
// it is in.
//
// This is the driver's report of what is actually running, and the only source of
// that information — the server persists nothing about it.
func (d *Driver) Observe(ctx context.Context) ([]driver.Instance, error) {
	containers, err := d.containers(ctx, "")
	if err != nil {
		return nil, err
	}

	instances := make([]driver.Instance, 0, len(containers))
	for _, c := range containers {
		version, _ := strconv.Atoi(c.Labels[LabelVersion])

		instance := driver.Instance{
			ID:       c.ID,
			Workload: c.Labels[LabelWorkload],
			SpecHash: c.Labels[LabelSpecHash],
			Version:  version,
			State:    state(c.State),
		}

		// The summary carries no exit code or start time, so anything that has
		// stopped needs an inspect to find out how it ended. Running containers
		// are left alone to keep the common path to a single API call.
		if instance.State == driver.StateExited || instance.State == driver.StateFailed {
			d.inspect(ctx, &instance)
		}

		instances = append(instances, instance)
	}

	return instances, nil
}

// Watch returns a channel of events describing changes to the containers the
// driver owns, so that the caller can reconcile sooner than its next scheduled
// pass.
//
// The returned channel is closed when ctx is cancelled or the underlying event
// stream fails. Events are a hint only: a caller that misses one still converges
// on its next pass, so a failed stream degrades responsiveness rather than
// correctness.
func (d *Driver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	messages, errs := d.client.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("label", LabelWorkload),
		),
	})

	out := make(chan driver.Event)

	go func() {
		defer close(out)

		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errs:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
					d.logger.With("error", err).Warn("docker event stream ended")
				}

				return
			case msg := <-messages:
				workload := msg.Actor.Attributes[LabelWorkload]
				if workload == "" {
					continue
				}

				select {
				case out <- driver.Event{Workload: workload}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// Logs returns the combined output of every container the driver holds for the
// named workload, limited to the last tail lines of each.
func (d *Driver) Logs(ctx context.Context, workload string, tail int) (string, error) {
	containers, err := d.containers(ctx, workload)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for _, c := range containers {
		logs, err := d.client.ContainerLogs(ctx, c.ID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Tail:       strconv.Itoa(tail),
		})
		if err != nil {
			return "", fmt.Errorf("failed to read container logs: %w", err)
		}

		// Docker multiplexes stdout and stderr into a single framed stream for
		// containers without a TTY, so it has to be demultiplexed rather than
		// copied straight out.
		if _, err = stdcopy.StdCopy(&out, &out, logs); err != nil {
			_ = logs.Close()
			return "", fmt.Errorf("failed to read container logs: %w", err)
		}

		if err = logs.Close(); err != nil {
			return "", fmt.Errorf("failed to close container logs: %w", err)
		}
	}

	return out.String(), nil
}

// containers returns the summaries of the containers the driver owns. When
// workload is empty every owned container is returned, otherwise only those
// belonging to the named workload.
func (d *Driver) containers(ctx context.Context, workload string) ([]container.Summary, error) {
	label := LabelWorkload
	if workload != "" {
		label = LabelWorkload + "=" + workload
	}

	containers, err := d.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", label)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	return containers, nil
}

// inspect fills in the fields that only a full container inspection carries.
// Failures are logged and swallowed: a missing exit code degrades the instance's
// detail but shouldn't fail the whole observation, and the container may simply
// have been removed between listing and inspecting.
func (d *Driver) inspect(ctx context.Context, instance *driver.Instance) {
	details, err := d.client.ContainerInspect(ctx, instance.ID)
	if err != nil {
		d.logger.With("container", instance.ID, "error", err).Debug("failed to inspect container")
		return
	}

	if details.State == nil {
		return
	}

	instance.ExitCode = details.State.ExitCode

	if startedAt, err := time.Parse(time.RFC3339Nano, details.State.StartedAt); err == nil {
		instance.StartedAt = startedAt
	}

	// A container that exited non-zero is a failure however Docker labels its
	// status, so the exit code has the final say.
	if instance.State == driver.StateExited && instance.ExitCode != 0 {
		instance.State = driver.StateFailed
	}
}

func (d *Driver) ensureImage(ctx context.Context, ref string) error {
	images, err := d.client.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", ref)),
	})
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	if len(images) > 0 {
		return nil
	}

	d.logger.With("image", ref).Debug("pulling image")

	pull, err := d.client.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	defer pull.Close()

	// The pull only runs to completion while its progress stream is being read,
	// so the body has to be drained even though nothing here reports progress.
	if _, err = io.Copy(io.Discard, pull); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	return nil
}

func state(status container.ContainerState) driver.State {
	switch status {
	case container.StateRunning, container.StateRestarting:
		return driver.StateRunning
	case container.StateCreated, container.StatePaused:
		return driver.StatePending
	case container.StateRemoving:
		// Being removed is orca's own doing — a replacement or a delete in
		// progress — so it is reported as terminating rather than as a failure.
		return driver.StateTerminating
	case container.StateExited:
		return driver.StateExited
	case container.StateDead:
		// Docker could not remove the container and will retry when the daemon
		// restarts. Nothing orca can do will move it on, so it stays a failure.
		return driver.StateFailed
	default:
		return driver.StatePending
	}
}

func environment(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}

	// Docker preserves the order it's given, so sorting keeps a container's
	// configuration byte-identical across restarts of the same specification.
	slices.Sort(out)

	return out
}

// containerName builds a predictable, human-readable container name. The version
// suffix keeps a replacement from colliding with the container it replaces while
// the old one is still being removed.
func containerName(workload string, version int) string {
	return fmt.Sprintf("orca-%s-%d", workload, version)
}
