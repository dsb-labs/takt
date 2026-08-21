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

	if len(s.Env) > 0 {
		spec.Env = new(s.Env)
	}
	if len(s.Ports) > 0 {
		mappings := make([]api.PortMapping, 0, len(s.Ports))
		for _, port := range s.Ports {
			mapping := api.PortMapping{To: port.To}

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
			mounts = append(mounts, api.VolumeMount{Name: mount.Name, To: mount.To})
		}

		spec.Volumes = &mounts
	}

	if s.Container != nil {
		spec.Container = &api.ContainerSpec{Image: s.Container.Image}

		if len(s.Container.Command) > 0 {
			spec.Container.Command = new(s.Container.Command)
		}
	}

	if s.Exec != nil {
		spec.Exec = &api.ExecSpec{Command: s.Exec.Command}
	}

	return spec
}
