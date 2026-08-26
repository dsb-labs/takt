// Package wire maps between the specification the HTTP API speaks and the canonical
// one the rest of orca reasons about.
//
// It exists so that the generated wire types stay inside the two packages that have
// business with them: the server's HTTP API and the client. Everything else — the
// service, the drivers, the reconciler, the database — holds a manifest.Spec, which
// is the shape orca stores, hashes and validates.
//
// The package sits beside the generated types rather than inside either caller
// because both directions are needed by both of them. The server returns a
// specification in its responses and the client reads one back out of them.
package wire

import (
	"time"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// ToSpec maps a wire specification onto the canonical shape.
//
// It exists so that anything holding the wire form — the server receiving a request,
// a client reading a response — can validate it against the same rules a manifest is
// held to, rather than each side growing its own.
//
// The defaults are resolved by the canonical package before the specification is
// returned, so a workload reaching orca over HTTP means what the same workload
// written as a manifest file means.
func ToSpec(spec api.WorkloadSpec) manifest.Spec {
	out := manifest.Spec{
		Version: spec.Version,
		Name:    spec.Name,
	}

	out.Schedule = toSchedule(spec.Schedule)
	out.Restart = toRestart(spec.Restart)

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

	out.Health = toHealth(spec.Health)
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
	}

	if spec.Exec != nil {
		out.Exec = &manifest.Exec{Command: spec.Exec.Command}
	}

	out.Defaults()

	return out
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

	if mount.Name != nil {
		out.Name = *mount.Name
	}
	if mount.Secret != nil {
		out.Secret = *mount.Secret
	}
	if mount.Var != nil {
		out.Var = *mount.Var
	}
	if mount.Signal != nil {
		out.Signal = manifest.Signal(*mount.Signal)
	}

	return out
}

// toHealth maps a wire health check onto the canonical shape.
//
// A duration that doesn't parse is recorded rather than returned, because this is
// also the path a client uses to read a workload back, where an error about a value
// the server already accepted would be nothing the caller could act on. Validation
// reports it instead.
func toHealth(spec *api.HealthSpec) *manifest.Health {
	if spec == nil {
		return nil
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

	for _, field := range []struct {
		name  string
		value *string
		into  *time.Duration
	}{
		{"interval", spec.Interval, &health.Interval},
		{"timeout", spec.Timeout, &health.Timeout},
		{"startPeriod", spec.StartPeriod, &health.StartPeriod},
	} {
		if field.value == nil || *field.value == "" {
			continue
		}

		if parsed, err := time.ParseDuration(*field.value); err == nil {
			*field.into = parsed
		} else {
			health.Invalid = append(health.Invalid, field.name)
		}
	}

	return &health
}

// toResources maps wire resource limits onto the canonical shape.
//
// The memory size is carried across as written rather than parsed here, so a value
// that does not parse survives to be reported by validation instead of erroring on
// the path a client uses to read a workload back.
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

// toRestart maps a wire restart policy onto the canonical shape.
//
// A nil policy still produces one, because every workload has an answer to what
// happens when it ends. What that answer is comes from the defaults ToSpec resolves.
//
// A delay that does not parse is recorded rather than returned, because this is also
// the path a client uses to read a workload back, where an error about a value the
// server already accepted would be nothing the caller could act on. Validation reports
// it instead.
func toRestart(spec *api.RestartSpec) *manifest.Restart {
	restart := new(manifest.Restart)

	if spec != nil {
		if spec.Policy != nil {
			restart.Policy = manifest.RestartPolicy(*spec.Policy)
		}
		if spec.Attempts != nil {
			restart.Attempts = *spec.Attempts
		}
		if spec.Delay != nil && *spec.Delay != "" {
			if parsed, err := time.ParseDuration(*spec.Delay); err == nil {
				restart.Delay = parsed
			} else {
				restart.InvalidDelay = *spec.Delay
			}
		}
	}

	return restart
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

	spec.Schedule = fromSchedule(s.Schedule)
	if len(s.Labels) > 0 {
		spec.Labels = new(s.Labels)
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

	if mount.Name != "" {
		out.Name = new(mount.Name)
	}
	if mount.Secret != "" {
		out.Secret = new(mount.Secret)
	}
	if mount.Var != "" {
		out.Var = new(mount.Var)
	}
	if mount.Signal != "" {
		out.Signal = new(api.MountSignal(mount.Signal))
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
