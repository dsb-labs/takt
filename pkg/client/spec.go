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

	if s.Container != nil {
		spec.Container = &api.ContainerSpec{Image: s.Container.Image}

		if len(s.Container.Env) > 0 {
			spec.Container.Env = new(s.Container.Env)
		}
		if len(s.Container.Ports) > 0 {
			spec.Container.Ports = new(s.Container.Ports)
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

// newSpec maps a wire specification onto the canonical shape.
func newSpec(spec api.WorkloadSpec) manifest.Spec {
	out := manifest.Spec{
		Version: spec.Version,
		Name:    spec.Name,
	}

	if spec.Schedule != nil {
		out.Schedule = *spec.Schedule
	}
	if spec.Labels != nil {
		out.Labels = *spec.Labels
	}

	if spec.Container != nil {
		out.Container = &manifest.Container{Image: spec.Container.Image}

		if spec.Container.Env != nil {
			out.Container.Env = *spec.Container.Env
		}
		if spec.Container.Ports != nil {
			out.Container.Ports = *spec.Container.Ports
		}
	}

	if spec.Script != nil {
		out.Script = new(manifest.Script)

		if spec.Script.Source != nil {
			out.Script.Source = *spec.Script.Source
		}
		if spec.Script.Raw != nil {
			out.Script.Raw = *spec.Script.Raw
		}
	}

	return out
}
