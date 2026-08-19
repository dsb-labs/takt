package manifest

import (
	"github.com/dsb-labs/orca/internal/generated/api"
)

type (
	// The Runtime type names the runtime a workload is run by, which determines
	// which of a specification's runtime blocks is used.
	Runtime string

	// The Spec type describes the desired state of a workload.
	//
	// It is the canonical shape of a workload throughout orca's public API: what
	// Parse produces from a manifest file, and what the client submits. Optional
	// values are plain zero values rather than pointers, so callers can build one
	// by hand without ceremony.
	Spec struct {
		// The manifest schema version. Must be "v1".
		Version string
		// The name that identifies the workload.
		Name string
		// The cron expression describing when the workload should run. Accepted
		// and stored, but not yet acted on.
		Schedule string
		// Arbitrary key-value pairs attached to the workload.
		Labels map[string]string
		// The container to run. Exactly one runtime must be set.
		Container *Container
		// The script to run. Exactly one runtime must be set.
		Script *Script
	}

	// The Container type describes the container a workload runs.
	Container struct {
		// The image reference to run.
		Image string
		// Environment variables set inside the container.
		Env map[string]string
		// The ports to publish.
		Ports []Port
	}

	// The Port type describes a port to publish.
	Port struct {
		// The port the workload listens on inside its runtime.
		To int
		// The host port that reaches it. Left unset to have one allocated, which is
		// the usual case: the workload keeps a port of its own and callers read the
		// allocated one back from the workload.
		From int
	}

	// The Script type describes the script a workload runs. Exactly one of Source
	// and Raw must be set.
	Script struct {
		// The URL the script is fetched from.
		Source string
		// The script body, given inline.
		Raw string
	}
)

const (
	// RuntimeContainer is the runtime that runs a workload as a container.
	RuntimeContainer Runtime = "container"
	// RuntimeScript is the runtime that runs a workload as a script.
	RuntimeScript Runtime = "script"
)

// NewSpec maps a wire specification onto the canonical shape.
//
// It exists so that anything holding the wire form — the server receiving a request,
// a client reading a response — can validate it against the same rules a manifest is
// held to, rather than each side growing its own.
func NewSpec(spec api.WorkloadSpec) Spec {
	out := Spec{
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
		out.Container = &Container{Image: spec.Container.Image}

		if spec.Container.Env != nil {
			out.Container.Env = *spec.Container.Env
		}
		if spec.Container.Ports != nil {
			out.Container.Ports = make([]Port, 0, len(*spec.Container.Ports))
			for _, mapping := range *spec.Container.Ports {
				port := Port{To: mapping.To}
				if mapping.From != nil {
					port.From = *mapping.From
				}

				out.Container.Ports = append(out.Container.Ports, port)
			}
		}
	}

	if spec.Script != nil {
		out.Script = new(Script)

		if spec.Script.Source != nil {
			out.Script.Source = *spec.Script.Source
		}
		if spec.Script.Raw != nil {
			out.Script.Raw = *spec.Script.Raw
		}
	}

	return out
}
