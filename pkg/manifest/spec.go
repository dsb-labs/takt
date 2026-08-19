package manifest

import (
	"time"

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
		// How to tell whether the workload is working, rather than merely started.
		Health *Health
		// The container to run. Exactly one runtime must be set.
		Container *Container
		// The script to run. Exactly one runtime must be set.
		Script *Script
	}

	// The Health type describes how to tell whether a workload is working.
	//
	// The timing fields mean the same thing for any runtime; the probe fields do not,
	// and one a runtime cannot perform is rejected rather than ignored.
	Health struct {
		// The path to request, when the workload should be checked over HTTP.
		HTTP string
		// Whether to check that the port merely accepts a connection.
		TCP bool
		// Which of the workload's ports to check, named as the port inside the
		// workload. Only needed when it publishes more than one.
		Port int
		// How often to perform the check.
		Interval time.Duration
		// How long a single check may take before it counts as failed.
		Timeout time.Duration
		// How many consecutive failures mark the workload as failed.
		Retries int
		// How long after starting to allow before failures are counted.
		//
		// The yaml tag is explicit because this is the first multi-word manifest key:
		// yaml.v3 matches against the lowercased Go field name, so without it the
		// manifest would have to spell this "startperiod" while the wire format spells
		// it "startPeriod".
		StartPeriod time.Duration `yaml:"startPeriod"`

		// The timing fields that were present but unparseable, reported by
		// validation so that a typo is an error rather than a silent default.
		invalid []string
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
	// DefaultHealthInterval is how often a check runs when the manifest doesn't say.
	DefaultHealthInterval = 10 * time.Second
	// DefaultHealthTimeout is how long a single check may take when unspecified.
	DefaultHealthTimeout = 2 * time.Second
	// DefaultHealthRetries is how many consecutive failures mark a workload failed
	// when unspecified.
	DefaultHealthRetries = 3
	// DefaultHealthStartPeriod is how long a workload is given to become ready before
	// failures are counted, when unspecified.
	DefaultHealthStartPeriod = 30 * time.Second
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

	out.Health = newHealth(spec.Health)

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

// newHealth maps a wire health check onto the canonical shape.
//
// A duration that doesn't parse is recorded rather than returned, because this is
// also the path a client uses to read a workload back, where an error about a value
// the server already accepted would be nothing the caller could act on. Validation
// reports it instead.
func newHealth(spec *api.HealthSpec) *Health {
	if spec == nil {
		return nil
	}

	var health Health

	if spec.HTTP != nil {
		health.HTTP = *spec.HTTP
	}
	if spec.TCP != nil {
		health.TCP = *spec.TCP
	}
	if spec.Port != nil {
		health.Port = *spec.Port
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
			health.invalid = append(health.invalid, field.name)
		}
	}

	health.defaults()

	return &health
}

// defaults fills in the timing fields the manifest left unset, so that everything
// downstream works with resolved values rather than repeating the question of what an
// unset interval means.
//
// This runs however a Spec was built — decoded from YAML or converted from the wire —
// because a default that only applied to one of those would make the same manifest
// behave differently depending on how it reached the server.
func (h *Health) defaults() {
	if h.Interval == 0 {
		h.Interval = DefaultHealthInterval
	}
	if h.Timeout == 0 {
		h.Timeout = DefaultHealthTimeout
	}
	if h.Retries == 0 {
		h.Retries = DefaultHealthRetries
	}
	if h.StartPeriod == 0 {
		h.StartPeriod = DefaultHealthStartPeriod
	}
}

// WireHealth maps the canonical health check onto the wire format.
func WireHealth(health *Health) *api.HealthSpec {
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
	if health.Port != 0 {
		spec.Port = new(health.Port)
	}

	return &spec
}
