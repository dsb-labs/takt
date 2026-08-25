package client

import (
	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// wireSpec maps a canonical specification onto the type the API accepts.
//
// Empty optional values are sent as absent rather than as empty ones, so that a
// specification submitted twice hashes identically on the server and doesn't read
// as a change.
func wireSpec(s manifest.Spec) api.WorkloadSpec {
	spec := api.WorkloadSpec{
		Version: s.Version,
		Name:    s.Name,
	}

	spec.Schedule = manifest.WireSchedule(s.Schedule)
	if len(s.Labels) > 0 {
		spec.Labels = new(s.Labels)
	}

	spec.Restart = manifest.WireRestart(s.Restart)
	spec.Health = manifest.WireHealth(s.Health)
	spec.Resources = manifest.WireResources(s.Resources)

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
			mounts = append(mounts, wireMount(mount))
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

// wireMount maps a canonical mount onto the type the API accepts.
//
// Only the source the mount names is sent. The others are absent rather than empty,
// for the same reason an unset host port is: a specification submitted twice has to
// hash identically on the server, and a mount that named a volume must encode exactly
// as it did before secrets could be mounted at all.
func wireMount(mount manifest.VolumeMount) api.VolumeMount {
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
