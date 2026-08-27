// Package docker provides the driver that runs orca workloads as Docker containers.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/docker/go-units"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/telemetry"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// Name is how this driver identifies itself, and is what the server maps a
// workload's runtime onto when deciding which driver runs it.
const Name = "container"

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
	// LabelAttempt names the container label holding which attempt at running a
	// workload a container is.
	//
	// The container's name carries it too, but the label is what the driver reads. A
	// name is an output of this driver rather than an input to it, so nothing parses
	// one back to learn what it means.
	//
	// It is also how a retained container is recognised. Docker fixes a container's
	// labels when it is created and offers no way to change them, so retention cannot
	// be written onto a container once something has replaced it. It does not need to
	// be: a container is superseded exactly when the workload has another with a
	// higher attempt, which is a comparison rather than a mark.
	LabelAttempt = "orca.attempt"
)

var (
	// ErrNotContainerWorkload is returned when NewWorkload is given a stored
	// workload whose specification carries no container block.
	ErrNotContainerWorkload = errors.New("workload does not describe a container")
)

type (
	// The Driver type runs workloads as Docker containers.
	Driver struct {
		logger      *slog.Logger
		client      Client
		bind        string
		configFile  string
		tracer      trace.Tracer
		instruments instruments
	}

	// The Config type contains fields used to construct a Driver.
	Config struct {
		// The logger used for driver lifecycle events.
		Logger *slog.Logger
		// The client used to talk to the Docker daemon.
		Client Client
		// The address a container's host ports are published on. Empty publishes on
		// loopback, which is the narrower of the two things this can mean.
		Bind string
		// The docker credential file registry credentials are resolved from. Empty
		// reads docker's own default location, decided when a pull happens rather
		// than here.
		ConfigFile string
		// The meter the driver's instruments are created from. May be nil, in
		// which case nothing is recorded.
		Meter metric.Meter
		// The tracer spans are created from. May be nil, in which case no spans
		// are recorded.
		Tracer trace.Tracer
	}
)

// The address a container's ports are published on when the configuration names
// none.
//
// Loopback rather than every interface, and deliberately not docker's own default:
// docker reads an empty host address as every interface, so a driver that passed one
// through would publish a workload to the network whenever a caller forgot to say.
const defaultBind = "127.0.0.1"

// Name returns the name this driver is registered under.
func (d *Driver) Name() string {
	return Name
}

// New returns a Driver that runs containers through the client in config.
func New(config Config) *Driver {
	bind := config.Bind
	if bind == "" {
		bind = defaultBind
	}

	return &Driver{
		logger:      config.Logger.With("component", "driver", "driver", "docker"),
		client:      config.Client,
		bind:        bind,
		configFile:  config.ConfigFile,
		tracer:      telemetry.Tracer(config.Tracer),
		instruments: newInstruments(config.Meter),
	}
}

// Start creates and starts a container for the given workload, pulling its image
// first when it isn't already present locally.
//
// The container is created with orca's ownership labels, which is the only record
// that ties it back to a workload — the server stores no container identifier.
// Docker's own restart policy is deliberately left unset: orca owns restart
// decisions so that they can be paced by its own backoff and remain visible in
// the workload's observed state.
func (d *Driver) Start(ctx context.Context, w driver.Workload) (string, error) {
	spec := w.Spec.Container
	if spec == nil {
		return "", ErrNotContainerWorkload
	}

	if err := d.ensureImage(ctx, spec.Image, spec.Pull); err != nil {
		return "", err
	}

	exposed, bindings := portBindings(d.bind, w.Ports)

	// Which attempt this is, read from what the driver already holds. A retained
	// container from the previous attempt keeps its name, so a replacement has to be
	// named something else or docker refuses to create it.
	held, err := d.containers(ctx, w.Name)
	if err != nil {
		return "", err
	}

	attempt := nextAttempt(held)

	labels := make(map[string]string, len(w.Labels)+4)
	for k, v := range w.Labels {
		labels[k] = v
	}
	labels[LabelWorkload] = w.Name
	labels[LabelSpecHash] = w.SpecHash
	labels[LabelVersion] = strconv.Itoa(w.Version)
	labels[LabelAttempt] = strconv.Itoa(attempt)

	limits, err := resources(w.Spec.Resources)
	if err != nil {
		return "", err
	}

	created, err := d.client.ContainerCreate(ctx,
		&container.Config{
			Image: spec.Image,
			// Nil rather than empty when the workload names no command, so the image
			// keeps the one it declares. An empty slice would replace it with nothing.
			Cmd:          spec.Command,
			User:         spec.User,
			Env:          environment(w.Env),
			Labels:       labels,
			ExposedPorts: exposed,
		},
		&container.HostConfig{
			PortBindings: bindings,
			Mounts:       mounts(w.Volumes),
			Resources:    limits,
			// Applied here rather than carried in the specification, so that it
			// cannot reach the specification hash and replace every running
			// instance on upgrade. It is unconditional because it breaks
			// essentially nothing that is not already doing something suspect.
			SecurityOpt:    []string{"no-new-privileges"},
			CapAdd:         capabilities(spec.CapAdd),
			CapDrop:        capabilities(spec.CapDrop),
			ReadonlyRootfs: spec.ReadOnly,
		},
		nil, nil,
		containerName(w.Name, w.Version, attempt),
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

// Stop stops every container the driver holds for the named workload, keeping the most
// recently created one so that its output can still be read.
//
// The workload's identifier is not used. This driver's ownership is expressed in
// container labels rather than on disk, so the name is all it needs to find its work.
//
// One container is retained because this is the path a replacement and a restart both
// take: the reconciler stops a workload before it starts it, so removing everything
// here is what used to discard the output of the attempt that just failed. Docker keeps
// the logs of a container that exists, so retaining the container is all it takes and
// orca does not have to invent a retention policy or a size cap of its own.
//
// Exactly one is retained, and every other container the workload has — including
// whatever the previous stop retained — is removed in the same call. Unbounded
// retention would be a disk leak on a workload crashing in a loop, and pruning here
// rather than once a replacement has settled means the invariant holds at every moment
// rather than between passes. What that discards is the older corpse, where the one
// being kept is more recent evidence of the same failure.
//
// Removal is forced because ContainerStop only asks the container to stop and
// returns before it necessarily has: an unforced remove races that shutdown and
// fails with "container is running", which would leave the container behind for
// every future pass to trip over. The stop is still issued first so the container
// gets its grace period rather than being killed outright.
func (d *Driver) Stop(ctx context.Context, _, workload string) error {
	containers, err := d.containers(ctx, workload)
	if err != nil {
		return err
	}

	for _, c := range containers {
		if err = d.client.ContainerStop(ctx, c.ID, container.StopOptions{}); err != nil {
			return fmt.Errorf("failed to stop container: %w", err)
		}

		d.logger.With("workload", workload, "container", c.ID).Debug("container stopped")
	}

	keep := newest(containers)

	for _, c := range containers {
		if c.ID == keep {
			continue
		}

		if err = d.remove(ctx, c.ID); err != nil {
			return err
		}
	}

	if keep == "" {
		return nil
	}

	d.logger.With("workload", workload, "container", keep).Debug("retained a stopped container so its output survives")

	return nil
}

// Discard stops and removes everything the driver holds for the named workload,
// including whatever Stop retained.
//
// This is what a delete and an orphan take, where Stop is what a replacement takes. A
// retained container exists so that an operator can read why the previous attempt
// failed; a workload nobody asked for has no such reader, and one left behind is a
// container the orphan sweep would find on every pass forever.
func (d *Driver) Discard(ctx context.Context, _, workload string) error {
	containers, err := d.containers(ctx, workload)
	if err != nil {
		return err
	}

	for _, c := range containers {
		if err = d.client.ContainerStop(ctx, c.ID, container.StopOptions{}); err != nil {
			return fmt.Errorf("failed to stop container: %w", err)
		}

		if err = d.remove(ctx, c.ID); err != nil {
			return err
		}

		d.logger.With("workload", workload, "container", c.ID).Debug("container discarded")
	}

	return nil
}

func (d *Driver) remove(ctx context.Context, id string) error {
	if err := d.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	return nil
}

// Signal sends the named signal to every container the driver holds for the named
// workload.
//
// This exists for a workload that mounts a value and asked to be told when it changes
// rather than replaced. Only a container that is running is signalled: docker refuses
// to signal one that has stopped, and a stopped container has nothing to reload.
//
// The workload's identifier is not used, for the same reason Stop does not use it.
func (d *Driver) Signal(ctx context.Context, _, workload, signal string) error {
	containers, err := d.containers(ctx, workload)
	if err != nil {
		return err
	}

	for _, c := range containers {
		if state(c.State) != driver.StateRunning {
			continue
		}

		if err = d.client.ContainerKill(ctx, c.ID, signal); err != nil {
			return fmt.Errorf("failed to signal container: %w", err)
		}

		d.logger.With("workload", workload, "container", c.ID, "signal", signal).Debug("container signalled")
	}

	return nil
}

// Observe returns an instance for every container the driver owns, whatever state
// it is in.
//
// This is the driver's report of what is actually running, and the only source of
// that information — the server persists nothing about it.
//
// A container something has already replaced is reported as retained, so that a caller
// deciding what to run leaves it out while one sweeping up orphans still finds it.
func (d *Driver) Observe(ctx context.Context) ([]driver.Instance, error) {
	return d.observe(ctx, "")
}

// ObserveWorkload reports every instance the driver is currently running for one
// workload, in the same terms as Observe.
//
// The daemon does the filtering, on the label every container the driver starts
// carries. Reading one workload therefore costs one query for that workload rather
// than a listing of everything on the host, which is what a read of a single
// workload used to pay.
//
// The identifier is unused here: a container records the workload's name in its
// labels, and that is what the filter matches. It is part of the signature because
// the exec driver keys its state on the identifier, and one interface serves both.
func (d *Driver) ObserveWorkload(ctx context.Context, _, name string) ([]driver.Instance, error) {
	return d.observe(ctx, name)
}

// observe reports the instances of one workload, or of every workload when the name
// is empty.
//
// Retention is decided within whatever was listed, which is correct either way:
// supersededBy groups by workload before choosing, so a listing narrowed to one
// workload reaches the same answer for it as a listing of the host would.
func (d *Driver) observe(ctx context.Context, name string) ([]driver.Instance, error) {
	containers, err := d.containers(ctx, name)
	if err != nil {
		return nil, err
	}

	superseded := supersededBy(containers)

	instances := make([]driver.Instance, 0, len(containers))
	for _, c := range containers {
		version, _ := strconv.Atoi(c.Labels[LabelVersion])

		instance := driver.Instance{
			ID:       c.ID,
			Workload: c.Labels[LabelWorkload],
			SpecHash: c.Labels[LabelSpecHash],
			Version:  version,
			State:    state(c.State),
			Ports:    instancePorts(c.Ports),
			Retained: superseded[c.ID],
			// When the container was created, which the summary carries and the
			// caller uses to tell a container that is up from one that has stayed up.
			// An inspect would give the moment it actually started, but the two differ
			// by the time it took to start — not by enough to be worth a call per
			// container on every pass. Overwritten below where an inspect happens
			// anyway.
			StartedAt: time.Unix(c.Created, 0),
		}

		// The summary carries no exit code or start time, so anything that has
		// stopped needs an inspect to find out how it ended. A running container is
		// inspected only when it reports a health check of its own, which the summary
		// mentions in its status text — the typed result lives on the inspection, and
		// the text is not a contract worth parsing. A container without a check stays
		// on the single-call path.
		if instance.State == driver.StateExited || instance.State == driver.StateFailed || hasHealthCheck(c.Status) {
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

// Logs writes the output of the containers the driver holds for the named workload to
// out, limited to the last Tail lines of each.
//
// Which containers those are is what options decides. The current attempt is read by
// default, and the retained one when Previous is asked for. The two are never combined:
// the driver holds the attempt now running and the one before it, so writing both would
// return two runs spliced together with nothing marking the boundary.
//
// Following keeps the read open until the container ends or the caller goes away.
// Cancellation is an ordinary way for that to happen, so a cancelled read reports no
// error: the caller who stopped listening already knows why the output stopped.
//
// The output is written as it is read rather than accumulated and returned. A
// workload's logs are unbounded in principle — a chatty container plus a generous
// tail is as much memory as the caller asks for — so holding the whole response
// before sending any of it would let one request decide how much the server uses.
func (d *Driver) Logs(ctx context.Context, out io.Writer, workload string, options driver.LogOptions) error {
	containers, err := d.containers(ctx, workload)
	if err != nil {
		return err
	}

	superseded := supersededBy(containers)

	for _, c := range containers {
		// A container nothing has replaced is the current attempt, and one that has
		// been replaced is the retained one. Asking for the previous output when the
		// workload has only ever run once writes nothing, which is the honest answer:
		// there is no earlier attempt to read.
		if superseded[c.ID] != options.Previous {
			continue
		}

		if err = d.containerLogs(ctx, out, c.ID, options); err != nil {
			return err
		}
	}

	return nil
}

func (d *Driver) containerLogs(ctx context.Context, out io.Writer, id string, options driver.LogOptions) error {
	request := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(options.Tail),
		Follow:     options.Follow,
	}

	// The daemon keeps a timestamp for every line it holds, so it can do the filtering
	// itself. RFC 3339 is one of the two formats it accepts, and the only one of them
	// that says what it means when read back out of a request.
	if !options.Since.IsZero() {
		request.Since = options.Since.Format(time.RFC3339Nano)
	}

	logs, err := d.client.ContainerLogs(ctx, id, request)
	if err != nil {
		return fmt.Errorf("failed to read container logs: %w", err)
	}
	// Closing releases the daemon's end of a followed stream. Without it a caller who
	// went away leaves the driver reading a container nobody is listening to.
	defer logs.Close()

	// Docker multiplexes stdout and stderr into a single framed stream for
	// containers without a TTY, so it has to be demultiplexed rather than
	// copied straight out.
	if _, err = stdcopy.StdCopy(out, out, logs); err != nil {
		// A cancelled read fails in whatever way the transport noticed first. That is
		// how a follow ends, so it is reported as the end rather than as a failure.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("failed to read container logs: %w", err)
	}

	return nil
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

	if details.State.Health != nil {
		instance.RuntimeHealth = details.State.Health.Status
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

// ensureImage makes the named image available under the workload's pull policy.
//
// The empty policy means manifest.PullMissing, so a specification written before
// the policy existed behaves as it always did. An always policy pulls without
// looking at what is held locally, since the point of asking for it is to fetch the
// tag's current content. A never policy must fail when the image is absent rather
// than falling through to a pull, or it is indistinguishable from missing.
func (d *Driver) ensureImage(ctx context.Context, ref string, policy manifest.PullPolicy) error {
	if policy != manifest.PullAlways {
		images, err := d.client.ImageList(ctx, image.ListOptions{
			Filters: filters.NewArgs(filters.Arg("reference", ref)),
		})
		if err != nil {
			return fmt.Errorf("failed to list images: %w", err)
		}

		if len(images) > 0 {
			return nil
		}

		if policy == manifest.PullNever {
			return fmt.Errorf("image %q is not present and the pull policy forbids pulling it", ref)
		}
	}

	auth, err := d.registryAuth(ref)
	if err != nil {
		return err
	}

	d.logger.With("image", ref).Debug("pulling image")

	// The measurement covers the drain below as well as the request: the pull is
	// only complete once its progress stream has been read to the end.
	ctx, span := d.tracer.Start(ctx, "image.pull",
		trace.WithAttributes(attribute.String("orca.image", ref)))
	defer span.End()

	started := time.Now()
	defer func() {
		d.instruments.pulls.Record(ctx, time.Since(started).Seconds(),
			metric.WithAttributes(attribute.String("image", ref)))
	}()

	pull, err := d.client.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: auth})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())

		return fmt.Errorf("failed to pull image: %w", err)
	}
	defer pull.Close()

	// The pull only runs to completion while its progress stream is being read,
	// so the body has to be drained even though nothing here reports progress.
	if _, err = io.Copy(io.Discard, pull); err != nil {
		span.SetStatus(codes.Error, err.Error())

		return fmt.Errorf("failed to pull image: %w", err)
	}

	return nil
}

// Digest asks the image's registry which digest the given reference currently
// resolves to.
//
// This is what folds a pull-always workload's image content into its specification
// hash: the workload service calls it whenever it computes the hash, so a rebuilt
// tag moves the hash and the instance is replaced through the ordinary stale path.
// The registry is asked with the credentials the docker credential file holds for
// it, so a private image resolves wherever a docker pull on the host would.
func (d *Driver) Digest(ctx context.Context, ref string) (string, error) {
	auth, err := d.registryAuth(ref)
	if err != nil {
		return "", err
	}

	inspect, err := d.client.DistributionInspect(ctx, ref, auth)
	if err != nil {
		return "", fmt.Errorf("failed to resolve image digest: %w", err)
	}

	return inspect.Descriptor.Digest.String(), nil
}

// hasHealthCheck reports whether a container summary mentions a health check.
//
// Docker reports health in the summary only inside the human-facing status text
// ("Up 6 seconds (unhealthy)"), so this recognises that a check exists in order to
// decide whether an inspection is worth making. The verdict itself is read from the
// inspection's typed field rather than from this text, which carries no guarantee
// about its wording.
func hasHealthCheck(status string) bool {
	return strings.Contains(status, "(health") || strings.Contains(status, "(healthy)") ||
		strings.Contains(status, "(unhealthy)")
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

// instancePorts reports the published ports docker says a container has, which is the
// observed counterpart to the allocation the server recorded.
func instancePorts(ports []container.Port) []driver.Port {
	if len(ports) == 0 {
		return nil
	}

	published := make([]driver.Port, 0, len(ports))
	for _, port := range ports {
		// An unpublished exposed port has no host side, so there is nothing for a
		// caller to reach and nothing worth reporting.
		if port.PublicPort == 0 {
			continue
		}

		published = append(published, driver.Port{
			Container: int(port.PrivatePort),
			Host:      int(port.PublicPort),
			Protocol:  port.Type,
		})
	}

	return published
}

// mounts converts resolved volumes into the bind mounts docker expects.
//
// A bind rather than a docker named volume, because the exec runtime needs a real path
// on the host whatever this driver does, and one mechanism serving both runtimes means
// a volume means the same thing wherever it is mounted. The cost is that the daemon
// has to share this filesystem, so a volume cannot be mounted into a container on a
// remote daemon.
func mounts(volumes []driver.Volume) []mount.Mount {
	if len(volumes) == 0 {
		return nil
	}

	out := make([]mount.Mount, 0, len(volumes))
	for _, volume := range volumes {
		out = append(out, mount.Mount{
			Type:   mount.TypeBind,
			Source: volume.Host,
			Target: volume.Target,
		})
	}

	return out
}

// portBindings converts resolved ports into the exposed set and host bindings docker
// expects, published on the given address.
//
// The address is what decides who can reach the workload, so it comes from the
// server's configuration rather than being fixed here. Publishing on every interface
// is a thing an operator can ask for and not a thing a driver assumes.
func portBindings(bind string, ports []driver.Port) (nat.PortSet, nat.PortMap) {
	if len(ports) == 0 {
		return nil, nil
	}

	exposed := make(nat.PortSet, len(ports))
	bindings := make(nat.PortMap, len(ports))

	for _, port := range ports {
		// A port that names no protocol is a TCP one, which is what every port was
		// before a specification could ask for the other. Publishing it under an
		// empty protocol would leave the workload unreachable at an address the
		// server reports.
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}

		key := nat.Port(strconv.Itoa(port.Container) + "/" + protocol)

		exposed[key] = struct{}{}
		bindings[key] = []nat.PortBinding{{
			HostIP:   bind,
			HostPort: strconv.Itoa(port.Host),
		}}
	}

	return exposed, bindings
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
//
// The attempt suffix is what makes retention possible. A restart at an unchanged
// version reuses the version, so the two components together are what stop a
// replacement colliding with the container it is replacing — which now outlives it
// rather than being removed on the way past.
func containerName(workload string, version, attempt int) string {
	return fmt.Sprintf("orca-%s-%d-%d", workload, version, attempt)
}

// nextAttempt reports which attempt at running a workload the next container is, given
// what the driver already holds for it.
//
// Counted from the highest attempt seen rather than from how many containers there are,
// so that removing one does not hand its number to the next container and collide with
// whatever still holds the name.
func nextAttempt(containers []container.Summary) int {
	highest := 0
	for _, c := range containers {
		if attempt, err := strconv.Atoi(c.Labels[LabelAttempt]); err == nil && attempt > highest {
			highest = attempt
		}
	}

	return highest + 1
}

// newest reports the container a workload most recently ran, which is the one worth
// keeping for its output.
//
// Ordered by attempt, falling back to creation time for a container from a server that
// wrote no attempt label. Docker does not promise an order from a list, so nothing here
// depends on one.
func newest(containers []container.Summary) string {
	var (
		id      string
		attempt = -1
		created int64
	)

	for _, c := range containers {
		this, err := strconv.Atoi(c.Labels[LabelAttempt])
		if err != nil {
			this = 0
		}

		if this > attempt || (this == attempt && c.Created > created) {
			id, attempt, created = c.ID, this, c.Created
		}
	}

	return id
}

// supersededBy reports which containers another container has replaced, keyed by
// identifier.
//
// A container is superseded when its workload has another with a higher attempt, which
// is what retention means here: docker fixes a container's labels at creation, so being
// replaced cannot be written onto the container and is derived by comparison instead.
func supersededBy(containers []container.Summary) map[string]bool {
	byWorkload := make(map[string][]container.Summary)
	for _, c := range containers {
		workload := c.Labels[LabelWorkload]
		byWorkload[workload] = append(byWorkload[workload], c)
	}

	superseded := make(map[string]bool, len(containers))
	for _, held := range byWorkload {
		current := newest(held)

		for _, c := range held {
			superseded[c.ID] = c.ID != current
		}
	}

	return superseded
}

// capabilities converts a specification's capability list into the type docker
// expects, or nil when the specification names none.
func capabilities(names []string) strslice.StrSlice {
	if len(names) == 0 {
		return nil
	}

	return strslice.StrSlice(names)
}

// resources converts a specification's resource limits into the cgroup settings
// docker expects. A limit the specification does not name is left at its zero value,
// which docker reads as unlimited.
//
// Validation proved the memory size parses, so an error here means the stored
// specification and the rules have diverged rather than that the operator made a
// mistake.
func resources(spec *manifest.Resources) (container.Resources, error) {
	if spec == nil {
		return container.Resources{}, nil
	}

	var limits container.Resources

	if spec.Memory != "" {
		memory, err := units.RAMInBytes(spec.Memory)
		if err != nil {
			return container.Resources{}, fmt.Errorf("failed to parse memory limit %q: %w", spec.Memory, err)
		}

		limits.Memory = memory
		// Swap is pinned to the memory limit so the limit is hard. Left alone,
		// docker lets the container swap up to twice the limit, and a limit the
		// workload can swap past does not mean what the manifest said.
		limits.MemorySwap = memory
	}

	if spec.CPU > 0 {
		// Docker expresses a CPU limit in billionths of a core, so half a core is
		// 500 million. Rounded rather than truncated so the workload gets the
		// nearest representable limit to the one it asked for.
		limits.NanoCPUs = int64(math.Round(spec.CPU * 1e9))
	}

	if spec.Pids > 0 {
		limits.PidsLimit = new(int64(spec.Pids))
	}

	return limits, nil
}
