// Package wire maps between the specification the HTTP API speaks and the canonical
// one the rest of takt reasons about.
//
// It exists so that the generated wire types stay inside the two packages that have
// business with them: the server's HTTP API and the client. Everything else — the
// service, the drivers, the reconciler, the database — holds a manifest.Spec, which
// is the shape takt stores, hashes and validates.
//
// The package sits beside the generated types rather than inside either caller
// because both directions are needed by both of them. The server returns a
// specification in its responses and the client reads one back out of them.
package wire

import (
	"fmt"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// ToSpec maps a wire specification onto the canonical shape.
//
// It exists so that anything holding the wire form — the server receiving a request,
// a client reading a response — can validate it against the same rules a manifest is
// held to, rather than each side growing its own.
//
// The defaults are resolved by the canonical package before the specification is
// returned, so a workload reaching takt over HTTP means what the same workload
// written as a manifest file means.
//
// A duration that does not parse is an error. The wire format spells one as a string,
// so this is the only place that reads it, and a specification carrying one takt
// cannot understand is not a specification it can run.
func ToSpec(spec api.WorkloadSpec) (manifest.Spec, error) {
	out := manifest.Spec{
		Version: spec.Version,
		Name:    spec.Name,
	}

	if spec.Count != nil {
		out.Count = *spec.Count
	}

	restart, err := toRestart(spec.Restart)
	if err != nil {
		return manifest.Spec{}, err
	}

	health, err := toHealth(spec.Health)
	if err != nil {
		return manifest.Spec{}, err
	}

	out.Schedule = toSchedule(spec.Schedule)
	out.Restart = restart
	out.Health = health

	if spec.Labels != nil {
		out.Labels = *spec.Labels
	}
	if spec.Env != nil {
		out.Env = *spec.Env
	}
	if spec.Ports != nil {
		out.Ports = make([]manifest.Port, 0, len(*spec.Ports))
		for _, mapping := range *spec.Ports {
			port := manifest.Port{To: mapping.To}
			if mapping.Name != nil {
				port.Name = *mapping.Name
			}
			if mapping.From != nil {
				port.From = *mapping.From
			}
			if mapping.Protocol != nil {
				port.Protocol = manifest.Protocol(*mapping.Protocol)
			}

			out.Ports = append(out.Ports, port)
		}
	}

	if spec.Volumes != nil {
		out.Volumes = make([]manifest.VolumeMount, 0, len(*spec.Volumes))
		for _, mount := range *spec.Volumes {
			out.Volumes = append(out.Volumes, ToVolumeMount(mount))
		}
	}

	out.Resources = toResources(spec.Resources)

	if spec.Container != nil {
		out.Container = &manifest.Container{Image: spec.Container.Image}

		if spec.Container.Pull != nil {
			out.Container.Pull = manifest.PullPolicy(*spec.Container.Pull)
		}
		if spec.Container.Command != nil {
			out.Container.Command = *spec.Container.Command
		}
		if spec.Container.User != nil {
			out.Container.User = *spec.Container.User
		}
		if spec.Container.ReadOnly != nil {
			out.Container.ReadOnly = *spec.Container.ReadOnly
		}
		if spec.Container.CapAdd != nil {
			out.Container.CapAdd = *spec.Container.CapAdd
		}
		if spec.Container.CapDrop != nil {
			out.Container.CapDrop = *spec.Container.CapDrop
		}
		if spec.Container.PidMode != nil {
			out.Container.PidMode = string(*spec.Container.PidMode)
		}
		if spec.Container.NetworkMode != nil {
			out.Container.NetworkMode = string(*spec.Container.NetworkMode)
		}
	}

	if spec.Exec != nil {
		out.Exec = &manifest.Exec{Command: spec.Exec.Command}
	}

	out.Defaults()

	return out, nil
}

// ToVolumeMount maps a wire mount onto the canonical shape.
//
// Exported so that a caller which only cares about what a workload mounts can convert
// those alone. Converting the whole specification through ToSpec allocates a restart
// policy and the rest of it, which is waste on a path asked about every workload on
// every reconciliation pass.
//
// The source fields are carried across as they were given rather than being resolved
// to a kind here. Which source a mount names is derived wherever it matters, so a
// mount naming none or naming two survives to be reported by validation instead of
// becoming a mount of something arbitrary.
func ToVolumeMount(mount api.VolumeMount) manifest.VolumeMount {
	out := manifest.VolumeMount{To: mount.To}

	if mount.From != nil {
		out.From = *mount.From
	}
	if mount.Name != nil {
		out.Name = *mount.Name
	}
	if mount.Secret != nil {
		out.Secret = *mount.Secret
	}
	if mount.Var != nil {
		out.Var = *mount.Var
	}
	if mount.Path != nil {
		out.Path = *mount.Path
	}
	if mount.Signal != nil {
		out.Signal = manifest.Signal(*mount.Signal)
	}
	if mount.ReadOnly != nil {
		out.ReadOnly = *mount.ReadOnly
	}
	if mount.Propagation != nil {
		out.Propagation = string(*mount.Propagation)
	}

	return out
}

// toHealth maps a wire health check onto the canonical shape, returning an error
// when one of the timing fields is not a duration.
func toHealth(spec *api.HealthSpec) (*manifest.Health, error) {
	if spec == nil {
		return nil, nil
	}

	var health manifest.Health

	if spec.HTTP != nil {
		health.HTTP = *spec.HTTP
	}
	if spec.TCP != nil {
		health.TCP = *spec.TCP
	}
	if spec.Port != nil {
		health.Port = manifest.PortRef(*spec.Port)
	}
	if spec.Retries != nil {
		health.Retries = *spec.Retries
	}

	interval, err := duration(spec.Interval)
	if err != nil {
		return nil, fmt.Errorf("failed to parse health check interval: %w", err)
	}

	health.Interval = interval

	timeout, err := duration(spec.Timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to parse health check timeout: %w", err)
	}

	health.Timeout = timeout

	startPeriod, err := duration(spec.StartPeriod)
	if err != nil {
		return nil, fmt.Errorf("failed to parse health check start period: %w", err)
	}

	health.StartPeriod = startPeriod

	return &health, nil
}

// duration parses an optional duration, which is zero when the field is absent or
// empty. The canonical package resolves the default for a timing field left unset.
func duration(value *string) (time.Duration, error) {
	if value == nil || *value == "" {
		return 0, nil
	}

	parsed, err := time.ParseDuration(*value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration", *value)
	}

	return parsed, nil
}

// toResources maps wire resource limits onto the canonical shape.
//
// The memory size is carried across as written rather than parsed, unlike a duration.
// The stored specification has to hold exactly what the manifest said, so the size is
// kept as text and validation proves it parses.
func toResources(spec *api.ResourcesSpec) *manifest.Resources {
	if spec == nil {
		return nil
	}

	var resources manifest.Resources

	if spec.Memory != nil {
		resources.Memory = *spec.Memory
	}
	if spec.CPU != nil {
		resources.CPU = *spec.CPU
	}
	if spec.Pids != nil {
		resources.Pids = *spec.Pids
	}

	return &resources
}

// toSchedule maps a wire schedule onto the canonical shape.
func toSchedule(spec *api.ScheduleSpec) *manifest.Schedule {
	if spec == nil {
		return nil
	}

	schedule := manifest.Schedule{Cron: spec.Cron}

	if spec.Overlap != nil {
		schedule.Overlap = manifest.OverlapPolicy(*spec.Overlap)
	}

	return &schedule
}

// toRestart maps a wire restart policy onto the canonical shape, returning an error
// when the delay is not a duration.
//
// A nil policy still produces one, because every workload has an answer to what
// happens when it ends. What that answer is comes from the defaults ToSpec resolves.
func toRestart(spec *api.RestartSpec) (*manifest.Restart, error) {
	restart := new(manifest.Restart)

	if spec == nil {
		return restart, nil
	}

	if spec.Policy != nil {
		restart.Policy = manifest.RestartPolicy(*spec.Policy)
	}
	if spec.Attempts != nil {
		restart.Attempts = *spec.Attempts
	}

	delay, err := duration(spec.Delay)
	if err != nil {
		return nil, fmt.Errorf("failed to parse restart delay: %w", err)
	}

	restart.Delay = delay

	return restart, nil
}

// FromSpec maps a canonical specification onto the type the API accepts.
//
// Empty optional values are sent as absent rather than as empty ones, so that a
// specification submitted twice hashes identically on the server and doesn't read
// as a change.
func FromSpec(s manifest.Spec) api.WorkloadSpec {
	spec := api.WorkloadSpec{
		Version: s.Version,
		Name:    s.Name,
	}

	if s.Count != 0 {
		spec.Count = new(s.Count)
	}

	spec.Schedule = fromSchedule(s.Schedule)
	if len(s.Labels) > 0 {
		spec.Labels = new(api.Labels(s.Labels))
	}

	spec.Restart = fromRestart(s.Restart)
	spec.Health = fromHealth(s.Health)
	spec.Resources = fromResources(s.Resources)

	if len(s.Env) > 0 {
		spec.Env = new(s.Env)
	}
	if len(s.Ports) > 0 {
		mappings := make([]api.PortMapping, 0, len(s.Ports))
		for _, port := range s.Ports {
			protocol := port.Protocol
			if protocol == "" {
				protocol = manifest.ProtocolTCP
			}

			mapping := api.PortMapping{To: port.To, Protocol: new(api.Protocol(protocol))}

			// An unnamed port is sent as absent rather than as an empty string, so
			// that a specification which names none encodes as it did before ports
			// could be named.
			if port.Name != "" {
				mapping.Name = new(port.Name)
			}

			// An unset host port is sent as absent rather than as zero, which is how
			// the server is asked to allocate one.
			if port.From != 0 {
				mapping.From = new(port.From)
			}

			mappings = append(mappings, mapping)
		}

		spec.Ports = &mappings
	}

	if len(s.Volumes) > 0 {
		mounts := make([]api.VolumeMount, 0, len(s.Volumes))
		for _, mount := range s.Volumes {
			mounts = append(mounts, FromVolumeMount(mount))
		}

		spec.Volumes = &mounts
	}

	if s.Container != nil {
		spec.Container = &api.ContainerSpec{Image: s.Container.Image}

		// The default is sent as absent rather than named, so a manifest that says
		// missing hashes the same as one that says nothing.
		if s.Container.Pull != "" && s.Container.Pull != manifest.PullMissing {
			spec.Container.Pull = new(api.PullPolicy(s.Container.Pull))
		}
		if len(s.Container.Command) > 0 {
			spec.Container.Command = new(s.Container.Command)
		}
		if s.Container.User != "" {
			spec.Container.User = new(s.Container.User)
		}
		if s.Container.ReadOnly {
			spec.Container.ReadOnly = new(s.Container.ReadOnly)
		}
		if len(s.Container.CapAdd) > 0 {
			spec.Container.CapAdd = new(s.Container.CapAdd)
		}
		if len(s.Container.CapDrop) > 0 {
			spec.Container.CapDrop = new(s.Container.CapDrop)
		}
		if s.Container.PidMode != "" {
			spec.Container.PidMode = new(api.ContainerSpecPidMode(s.Container.PidMode))
		}
		if s.Container.NetworkMode != "" {
			spec.Container.NetworkMode = new(api.ContainerSpecNetworkMode(s.Container.NetworkMode))
		}
	}

	if s.Exec != nil {
		spec.Exec = &api.ExecSpec{Command: s.Exec.Command}
	}

	return spec
}

// FromVolumeMount maps a canonical mount onto the type the API accepts.
//
// Only the source the mount names is sent. The others are absent rather than empty,
// for the same reason an unset host port is: a specification submitted twice has to
// hash identically on the server, and a mount that named a volume must encode exactly
// as it did before secrets could be mounted at all.
func FromVolumeMount(mount manifest.VolumeMount) api.VolumeMount {
	out := api.VolumeMount{To: mount.To}

	// Sent so that a caller reading a workload back sees where its volume lives, the
	// way it sees the host port the server settled on. A caller submitting one leaves
	// it empty, and the server resolves it from the volume that is named.
	if mount.From != "" {
		out.From = new(mount.From)
	}
	if mount.Name != "" {
		out.Name = new(mount.Name)
	}
	if mount.Secret != "" {
		out.Secret = new(mount.Secret)
	}
	if mount.Var != "" {
		out.Var = new(mount.Var)
	}
	if mount.Path != "" {
		out.Path = new(mount.Path)
	}
	if mount.Signal != "" {
		out.Signal = new(api.MountSignal(mount.Signal))
	}
	if mount.ReadOnly {
		out.ReadOnly = new(mount.ReadOnly)
	}
	if mount.Propagation != "" {
		out.Propagation = new(api.VolumeMountPropagation(mount.Propagation))
	}

	return out
}

// fromSchedule maps the canonical schedule onto the wire format.
func fromSchedule(schedule *manifest.Schedule) *api.ScheduleSpec {
	if schedule == nil {
		return nil
	}

	spec := api.ScheduleSpec{Cron: schedule.Cron}

	if schedule.Overlap != "" {
		spec.Overlap = new(api.OverlapPolicy(schedule.Overlap))
	}

	return &spec
}

// fromRestart maps the canonical restart policy onto the wire format.
func fromRestart(restart *manifest.Restart) *api.RestartSpec {
	if restart == nil {
		return nil
	}

	spec := api.RestartSpec{}

	if restart.Policy != "" {
		spec.Policy = new(api.RestartPolicy(restart.Policy))
	}
	if restart.Attempts > 0 {
		spec.Attempts = new(restart.Attempts)
	}
	if restart.Delay > 0 {
		spec.Delay = new(restart.Delay.String())
	}

	return &spec
}

// fromResources maps the canonical resource limits onto the wire format.
func fromResources(resources *manifest.Resources) *api.ResourcesSpec {
	if resources == nil {
		return nil
	}

	spec := api.ResourcesSpec{}

	if resources.Memory != "" {
		spec.Memory = new(resources.Memory)
	}
	if resources.CPU != 0 {
		spec.CPU = new(resources.CPU)
	}
	if resources.Pids != 0 {
		spec.Pids = new(resources.Pids)
	}

	return &spec
}

// fromHealth maps the canonical health check onto the wire format.
func fromHealth(health *manifest.Health) *api.HealthSpec {
	if health == nil {
		return nil
	}

	spec := api.HealthSpec{
		Interval:    new(health.Interval.String()),
		Timeout:     new(health.Timeout.String()),
		Retries:     new(health.Retries),
		StartPeriod: new(health.StartPeriod.String()),
	}

	if health.HTTP != "" {
		spec.HTTP = new(health.HTTP)
	}
	if health.TCP {
		spec.TCP = new(health.TCP)
	}
	if health.Port != "" {
		spec.Port = new(string(health.Port))
	}

	return &spec
}
