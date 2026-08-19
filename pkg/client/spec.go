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

	if s.Schedule != "" {
		spec.Schedule = new(s.Schedule)
	}
	if len(s.Labels) > 0 {
		spec.Labels = new(s.Labels)
	}

	spec.Restart = manifest.WireRestart(s.Restart)
	spec.Health = manifest.WireHealth(s.Health)

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

	if s.Container != nil {
		spec.Container = &api.ContainerSpec{Image: s.Container.Image}

		if len(s.Container.Command) > 0 {
			spec.Container.Command = new(s.Container.Command)
		}
		if len(s.Container.Env) > 0 {
			spec.Container.Env = new(s.Container.Env)
		}
	}

	if s.Script != nil {
		spec.Script = new(api.ScriptSpec)

		if s.Script.Source != "" {
			spec.Script.Source = new(s.Script.Source)
		}
		if s.Script.Raw != "" {
			spec.Script.Raw = new(s.Script.Raw)
		}
	}

	return spec
}
