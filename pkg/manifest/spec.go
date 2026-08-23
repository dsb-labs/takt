package manifest

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/dsb-labs/orca/internal/generated/api"
)

type (
	// The Runtime type names the runtime a workload is run by, which determines
	// which of a specification's runtime blocks is used.
	Runtime string

	// The RestartPolicy type names what happens when a workload's instance ends.
	RestartPolicy string

	// The OverlapPolicy type names what happens when an occurrence comes due while
	// the previous run is still going.
	OverlapPolicy string

	// The Schedule type describes when a workload runs.
	Schedule struct {
		// The cron expression, in the standard five-field form.
		Cron string
		// What to do when an occurrence comes due and the previous run has not
		// finished. Empty means OverlapReplace.
		Overlap OverlapPolicy
	}

	// The Restart type describes what happens when a workload's instance ends.
	Restart struct {
		// Whether to run the workload again. Empty means RestartAlways.
		Policy RestartPolicy
		// How many consecutive restarts to attempt before giving up. Zero means
		// orca keeps trying.
		Attempts int
		// How long to wait before the first restart, doubling on each consecutive
		// failure. Zero means DefaultRestartDelay.
		Delay time.Duration

		// The delay field as written when it did not parse, reported by validation
		// so that a typo is an error rather than a silent default.
		invalidDelay string
	}

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
		// When the workload runs, rather than running continuously. Nil runs it
		// continuously.
		Schedule *Schedule
		// Arbitrary key-value pairs attached to the workload.
		Labels map[string]string
		// The ports to publish, which is how the workload is reached. A runtime with
		// nothing to publish rejects them rather than ignoring them.
		Ports []Port
		// The environment variables set for the workload. A workload starts with only
		// these, rather than inheriting the server's own environment.
		Env map[string]string
		// The volumes to mount, and where the workload finds each one. Each names a
		// volume that must already exist.
		Volumes []VolumeMount
		// What to do when the workload's instance ends. Never nil once a Spec has
		// been through Parse or NewSpec, both of which resolve the defaults.
		Restart *Restart
		// How to tell whether the workload is working, rather than merely started.
		Health *Health
		// The container to run. Exactly one runtime must be set.
		Container *Container
		// The command to run on the host. Exactly one runtime must be set.
		Exec *Exec
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
		// The command to run, replacing the one the image declares. Empty runs what
		// the image already declares.
		Command []string
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

	// The Exec type describes the command a workload runs on the host.
	Exec struct {
		// The command to run, and its arguments. No shell is involved unless the
		// command names one.
		Command []string
	}

	// The VolumeMount type describes a volume a workload mounts, and where the
	// workload finds it.
	VolumeMount struct {
		// The volume to mount, which must already exist.
		Name string
		// Where the workload finds the volume, written the same way whichever
		// runtime runs it. Where it resolves to differs, because a container has a
		// filesystem of its own and a process on the host does not.
		//
		// For a container it is the path inside the container. For an exec workload
		// the volume is placed at this path relative to the workload's working
		// directory, and such a workload reaches it by that relative path: confining
		// the process so the absolute one resolved there would need privileges orca
		// does not have.
		To string
	}

	// The Volume type describes a volume, which is no more than its name. A volume
	// holds data and has nothing to configure.
	Volume struct {
		// The manifest schema version. Must be "v1".
		Version string
		// The name that identifies the volume.
		Name string
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
	// RuntimeExec is the runtime that runs a workload as a command on the host.
	RuntimeExec Runtime = "exec"
)

const (
	// RestartAlways restarts a workload whatever its exit code, which is what a
	// long-running service wants. It is the default.
	RestartAlways RestartPolicy = "always"
	// RestartOnFailure restarts a workload only when it exited non-zero, so one that
	// exits cleanly has finished its work.
	RestartOnFailure RestartPolicy = "on-failure"
	// RestartNever leaves a workload alone once it ends, whatever its exit code.
	RestartNever RestartPolicy = "never"
)

const (
	// OverlapReplace stops the running instance and starts the occurrence, so the
	// schedule is always honoured. It is the default.
	OverlapReplace OverlapPolicy = "replace"
	// OverlapSkip leaves the running instance alone and misses the occurrence, which
	// is what a job that must not be interrupted wants.
	OverlapSkip OverlapPolicy = "skip"
)

// DefaultRestartDelay is how long to wait before the first restart when the manifest
// does not say.
const DefaultRestartDelay = time.Second

// Restarts reports whether the policy calls for another run after an instance ended
// with the given exit code.
//
// An unknown policy restarts, which is the safe reading: validation rejects one, so
// reaching here with a value that is neither known nor empty means the stored
// specification and the rules have diverged. Continuing to run a service is a better
// failure than silently retiring it.
func (p RestartPolicy) Restarts(exitCode int) bool {
	switch p {
	case RestartNever:
		return false
	case RestartOnFailure:
		return exitCode != 0
	default:
		return true
	}
}

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

	out.Schedule = newSchedule(spec.Schedule)
	out.Restart = newRestart(spec.Restart)

	if spec.Labels != nil {
		out.Labels = *spec.Labels
	}
	if spec.Env != nil {
		out.Env = *spec.Env
	}
	if spec.Ports != nil {
		out.Ports = make([]Port, 0, len(*spec.Ports))
		for _, mapping := range *spec.Ports {
			port := Port{To: mapping.To}
			if mapping.From != nil {
				port.From = *mapping.From
			}

			out.Ports = append(out.Ports, port)
		}
	}

	if spec.Volumes != nil {
		out.Volumes = make([]VolumeMount, 0, len(*spec.Volumes))
		for _, mount := range *spec.Volumes {
			out.Volumes = append(out.Volumes, NewVolumeMount(mount))
		}
	}

	out.Health = newHealth(spec.Health)

	if spec.Container != nil {
		out.Container = &Container{Image: spec.Container.Image}

		if spec.Container.Command != nil {
			out.Container.Command = *spec.Container.Command
		}
	}

	if spec.Exec != nil {
		out.Exec = &Exec{Command: spec.Exec.Command}
	}

	return out
}

// NewVolumeMount maps a wire mount onto the canonical shape.
//
// The source fields are carried across as they were given rather than being resolved
// to a kind here. Which source a mount names is derived wherever it matters, so a
// mount naming none or naming two survives to be reported by validation instead of
// becoming a mount of something arbitrary.
func NewVolumeMount(mount api.VolumeMount) VolumeMount {
	out := VolumeMount{To: mount.To}

	if mount.Name != nil {
		out.Name = *mount.Name
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

// Parsed returns the schedule's expression ready to ask for occurrence times.
//
// Validation proves the expression parses, so an error here means the stored
// specification and the rules have diverged rather than that the operator made a
// mistake.
func (s *Schedule) Parsed() (cron.Schedule, error) {
	parsed, err := cron.ParseStandard(s.Cron)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cron expression %q: %w", s.Cron, err)
	}

	return parsed, nil
}

// newSchedule maps a wire schedule onto the canonical shape.
func newSchedule(spec *api.ScheduleSpec) *Schedule {
	if spec == nil {
		return nil
	}

	schedule := Schedule{Cron: spec.Cron}

	if spec.Overlap != nil {
		schedule.Overlap = OverlapPolicy(*spec.Overlap)
	}

	schedule.defaults()

	return &schedule
}

// newRestart maps a wire restart policy onto the canonical shape.
//
// A nil policy still produces one, since every workload has an answer to what happens
// when it ends and the answer is the default.
//
// A delay that does not parse is recorded rather than returned, because this is also
// the path a client uses to read a workload back, where an error about a value the
// server already accepted would be nothing the caller could act on. Validation reports
// it instead.
func newRestart(spec *api.RestartSpec) *Restart {
	restart := new(Restart)

	if spec != nil {
		if spec.Policy != nil {
			restart.Policy = RestartPolicy(*spec.Policy)
		}
		if spec.Attempts != nil {
			restart.Attempts = *spec.Attempts
		}
		if spec.Delay != nil && *spec.Delay != "" {
			if parsed, err := time.ParseDuration(*spec.Delay); err == nil {
				restart.Delay = parsed
			} else {
				restart.invalidDelay = *spec.Delay
			}
		}
	}

	restart.defaults()

	return restart
}

// defaults fills in what a schedule left unset.
//
// This runs however a Spec was built, decoded from YAML or converted from the wire,
// because a default that applied to only one of those would make the same manifest
// behave differently depending on how it reached the server.
func (s *Schedule) defaults() {
	if s.Overlap == "" {
		s.Overlap = OverlapReplace
	}
}

// defaults fills in what a restart policy left unset.
func (r *Restart) defaults() {
	if r.Policy == "" {
		r.Policy = RestartAlways
	}
	if r.Delay == 0 {
		r.Delay = DefaultRestartDelay
	}
}

// Restarts reports whether the policy calls for another run after an instance ended
// with the given exit code, having already been attempted the given number of times.
//
// Attempts are counted so that a workload can be told to give up. Zero means orca
// keeps trying, which is what a long-running service wants.
func (r *Restart) Restarts(exitCode, attempts int) bool {
	if r.Attempts > 0 && attempts >= r.Attempts {
		return false
	}

	return r.Policy.Restarts(exitCode)
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

// WireSchedule maps the canonical schedule onto the wire format.
func WireSchedule(schedule *Schedule) *api.ScheduleSpec {
	if schedule == nil {
		return nil
	}

	spec := api.ScheduleSpec{Cron: schedule.Cron}

	if schedule.Overlap != "" {
		spec.Overlap = new(api.OverlapPolicy(schedule.Overlap))
	}

	return &spec
}

// WireRestart maps the canonical restart policy onto the wire format.
func WireRestart(restart *Restart) *api.RestartSpec {
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
