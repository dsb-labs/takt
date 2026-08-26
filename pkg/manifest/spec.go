package manifest

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"

	"github.com/dsb-labs/orca/internal/generated/api"
)

type (
	// The Runtime type names the runtime a workload is run by, which determines
	// which of a specification's runtime blocks is used.
	Runtime string

	// The RestartPolicy type names what happens when a workload's instance ends.
	RestartPolicy string

	// The PullPolicy type names when the docker driver pulls a workload's image.
	PullPolicy string

	// The OverlapPolicy type names what happens when an occurrence comes due while
	// the previous run is still going.
	OverlapPolicy string

	// The Protocol type names the transport protocol a port is published on.
	Protocol string

	// The PortRef type names one of a workload's ports, written either as the name
	// the specification gave it or as the port inside the workload.
	//
	// One type rather than two fields, because both forms answer the same question
	// and a manifest that could write either would otherwise have to say which it
	// meant. A port name may not read as a number, so a reference matches at most
	// one of the two forms.
	PortRef string

	// The MountKind type names what a mount takes its contents from, which
	// determines which of a mount's source fields is used.
	MountKind string

	// The Signal type names the signal a workload is sent when a value it mounts
	// changes.
	//
	// Only a signal a program reloads on can be named. One that would stop the
	// workload is refused: the reconciler owns whether a workload runs, so a
	// manifest that could stop it would be deciding that behind the restart
	// policy's back.
	Signal string

	// The Schedule type describes when a workload runs.
	Schedule struct {
		// The cron expression, in the standard five-field form.
		Cron string `json:"cron"`
		// What to do when an occurrence comes due and the previous run has not
		// finished. Empty means OverlapReplace.
		Overlap OverlapPolicy `json:"overlap,omitempty"`
	}

	// The Restart type describes what happens when a workload's instance ends.
	Restart struct {
		// Whether to run the workload again. Empty means RestartAlways.
		Policy RestartPolicy `json:"policy,omitempty"`
		// How many consecutive restarts to attempt before giving up. Zero means
		// orca keeps trying.
		Attempts int `json:"attempts,omitempty"`
		// How long to wait before the first restart, doubling on each consecutive
		// failure. Zero means DefaultRestartDelay.
		Delay time.Duration `json:"delay,omitempty"`

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
	//
	// The JSON tags are explicit on every field because this encoding is what orca
	// stores a workload as, and what its specification hash covers. A hash that moves
	// replaces every running instance, so the encoding must not follow a Go field
	// name that somebody is free to rename. A field added later must be omitempty for
	// the same reason: without it, every workload that leaves the field unset encodes
	// differently than it did before the field existed.
	Spec struct {
		// The manifest schema version. Must be "v1".
		Version string `json:"version"`
		// The name that identifies the workload.
		Name string `json:"name"`
		// When the workload runs, rather than running continuously. Nil runs it
		// continuously.
		Schedule *Schedule `json:"schedule,omitempty"`
		// Arbitrary key-value pairs attached to the workload.
		Labels map[string]string `json:"labels,omitempty"`
		// The ports to publish, which is how the workload is reached. A runtime with
		// nothing to publish rejects them rather than ignoring them.
		Ports []Port `json:"ports,omitempty"`
		// The environment variables set for the workload. A workload starts with only
		// these, rather than inheriting the server's own environment.
		Env map[string]string `json:"env,omitempty"`
		// The volumes to mount, and where the workload finds each one. Each names a
		// volume that must already exist.
		Volumes []VolumeMount `json:"volumes,omitempty"`
		// What to do when the workload's instance ends. Never nil once a Spec has
		// been through Parse or NewSpec, both of which resolve the defaults.
		Restart *Restart `json:"restart,omitempty"`
		// How to tell whether the workload is working, rather than merely started.
		Health *Health `json:"health,omitempty"`
		// The resource limits the workload runs under. Nil applies none, so an
		// unlimited workload stays what it is today. A runtime that cannot enforce
		// them rejects them rather than ignoring them.
		Resources *Resources `json:"resources,omitempty"`
		// The container to run. Exactly one runtime must be set.
		Container *Container `json:"container,omitempty"`
		// The command to run on the host. Exactly one runtime must be set.
		Exec *Exec `json:"exec,omitempty"`
	}

	// The Health type describes how to tell whether a workload is working.
	//
	// The timing fields mean the same thing for any runtime; the probe fields do not,
	// and one a runtime cannot perform is rejected rather than ignored.
	Health struct {
		// The path to request, when the workload should be checked over HTTP.
		HTTP string `json:"http,omitempty"`
		// Whether to check that the port merely accepts a connection.
		TCP bool `json:"tcp,omitempty"`
		// Which of the workload's ports to check, written as the port's name or as
		// the port inside the workload. Only needed when it publishes more than one.
		Port PortRef `json:"port,omitempty"`
		// How often to perform the check.
		Interval time.Duration `json:"interval,omitempty"`
		// How long a single check may take before it counts as failed.
		Timeout time.Duration `json:"timeout,omitempty"`
		// How many consecutive failures mark the workload as failed.
		Retries int `json:"retries,omitempty"`
		// How long after starting to allow before failures are counted.
		//
		// The yaml tag is explicit because this is the first multi-word manifest key:
		// yaml.v3 matches against the lowercased Go field name, so without it the
		// manifest would have to spell this "startperiod" while the wire format spells
		// it "startPeriod".
		StartPeriod time.Duration `yaml:"startPeriod" json:"startPeriod,omitempty"`

		// The timing fields that were present but unparseable, reported by
		// validation so that a typo is an error rather than a silent default.
		invalid []string
	}

	// The Resources type describes the resource limits a workload runs under.
	//
	// A limit left at its zero value is not applied, so a workload naming only a
	// memory limit is otherwise as unlimited as one naming none.
	Resources struct {
		// The most memory the workload may use, written as a size such as "512m".
		//
		// Held as the operator wrote it rather than as a byte count, so that the
		// stored specification and its hash carry exactly what the manifest said.
		// Validation proves it parses, and the driver reads the number out.
		Memory string `json:"memory,omitempty"`
		// The most CPU the workload may use, in cores. Fractions are allowed, so
		// 0.5 is half a core.
		CPU float64 `json:"cpu,omitempty"`
		// The most processes and threads the workload may create.
		Pids int `json:"pids,omitempty"`
	}

	// The Container type describes the container a workload runs.
	//
	// The hardening fields live here rather than beside the runtime blocks because a
	// capability set and a root filesystem are container concepts: an exec workload
	// cannot even spell them, so nothing has to reject them for it.
	Container struct {
		// The image reference to run.
		Image string `json:"image"`
		// When the image is pulled. Empty means PullMissing, and stays empty rather
		// than being resolved to it: the default is left off the wire so that a
		// specification written before the field existed hashes as it always did.
		Pull PullPolicy `json:"pull,omitempty"`
		// The command to run, replacing the one the image declares. Empty runs what
		// the image already declares.
		Command []string `json:"command,omitempty"`
		// The user to run as, replacing the one the image declares. Any form docker
		// accepts: a name, a numeric identifier, or a "user:group" pair. Empty runs
		// as the user the image declares.
		User string `json:"user,omitempty"`
		// Whether the root filesystem is read-only. Mounted volumes and values are
		// separate mounts, so they stay writable and readable whatever this says.
		//
		// The yaml tags on this and the fields below are explicit for the reason
		// Health.StartPeriod's is: yaml.v3 matches against the lowercased Go field
		// name, and these keys are spelled camelCase on the wire.
		ReadOnly bool `yaml:"readOnly" json:"readOnly,omitempty"`
		// Kernel capabilities to grant beyond the runtime's default set.
		CapAdd []string `yaml:"capAdd" json:"capAdd,omitempty"`
		// Kernel capabilities to remove from the runtime's default set. Dropping ALL
		// and adding back what the workload needs is the hardened configuration.
		CapDrop []string `yaml:"capDrop" json:"capDrop,omitempty"`
	}

	// The Port type describes a port to publish.
	Port struct {
		// What the port is called, so that the rest of the manifest refers to it
		// rather than restating its number. A health check names one this way, and
		// so does another workload reaching this one.
		//
		// Optional, and only worth setting for a workload publishing more than one
		// port. Two ports may share a name only when they publish the same port on
		// different protocols, since that is one service named once rather than two
		// ports with nothing to tell them apart.
		Name string `json:"name,omitempty"`
		// The port the workload listens on inside its runtime.
		To int `json:"to"`
		// The host port that reaches it. Left unset to have one allocated, which is
		// the usual case: the workload keeps a port of its own and callers read the
		// allocated one back from the workload.
		From int `json:"from,omitempty"`
		// The transport protocol the port is published on. Defaults to TCP, and a
		// workload may publish the same port on both, since the two are separate
		// address spaces.
		Protocol Protocol `json:"protocol,omitempty"`
	}

	// The Exec type describes the command a workload runs on the host.
	Exec struct {
		// The command to run, and its arguments. No shell is involved unless the
		// command names one.
		Command []string `json:"command"`
	}

	// The VolumeMount type describes something a workload mounts, and where the
	// workload finds it.
	//
	// Exactly one source must be named, and which one it is decides what appears at
	// the path: a volume is a directory that outlives the workload, where a secret or
	// a variable is a file holding what orca holds under that name. The source is
	// derived from the field that is present rather than from a discriminator, as a
	// specification's runtime is.
	VolumeMount struct {
		// The volume to mount, which must already exist.
		Name string `json:"name,omitempty"`
		// The secret to mount as a file, which must already exist.
		Secret string `json:"secret,omitempty"`
		// The variable to mount as a file, which must already exist.
		Var string `json:"var,omitempty"`
		// Where the workload finds what is mounted, written the same way whichever
		// runtime runs it. Where it resolves to differs, because a container has a
		// filesystem of its own and a process on the host does not.
		//
		// For a container it is the path inside the container. For an exec workload
		// what is mounted is placed at this path relative to the workload's working
		// directory, and such a workload reaches it by that relative path: confining
		// the process so the absolute one resolved there would need privileges orca
		// does not have.
		To string `json:"to"`
		// The signal to send the workload when the mounted value changes, rather than
		// replacing its instance. Empty replaces the instance, which is what a
		// workload that reads a file once wants.
		//
		// Only a mounted secret or variable may name one. A volume holds whatever the
		// workload puts there, so there is no change orca could report.
		Signal Signal `json:"signal,omitempty"`
	}

	// The Volume type describes a volume, which is no more than its name. A volume
	// holds data and has nothing to configure.
	Volume struct {
		// The manifest schema version. Must be "v1".
		Version string `json:"version"`
		// The name that identifies the volume.
		Name string `json:"name"`
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
	// PullAlways pulls the image on every start, and its registry digest reaches the
	// specification hash so a rebuilt tag replaces the instance.
	PullAlways PullPolicy = "always"
	// PullMissing pulls the image only when it is not present on the host. It is the
	// default.
	PullMissing PullPolicy = "missing"
	// PullNever never pulls, and starting fails when the image is absent.
	PullNever PullPolicy = "never"
)

const (
	// OverlapReplace stops the running instance and starts the occurrence, so the
	// schedule is always honoured. It is the default.
	OverlapReplace OverlapPolicy = "replace"
	// OverlapSkip leaves the running instance alone and misses the occurrence, which
	// is what a job that must not be interrupted wants.
	OverlapSkip OverlapPolicy = "skip"
)

const (
	// ProtocolTCP publishes a port over TCP. It is the default.
	ProtocolTCP Protocol = "tcp"
	// ProtocolUDP publishes a port over UDP, which is a separate address space: a
	// port published over one protocol says nothing about the other.
	ProtocolUDP Protocol = "udp"
)

const (
	// MountVolume mounts a volume, which is a directory that outlives the workload.
	MountVolume MountKind = "volume"
	// MountSecret mounts a secret's value as a file.
	MountSecret MountKind = "secret"
	// MountVariable mounts a variable's value as a file.
	MountVariable MountKind = "var"
)

const (
	// SignalHUP is what most programs reload their configuration on.
	SignalHUP Signal = "SIGHUP"
	// SignalUSR1 is what a program reloading on a user-defined signal may use.
	SignalUSR1 Signal = "SIGUSR1"
	// SignalUSR2 is the other user-defined signal, for a program that already means
	// something else by the first.
	SignalUSR2 Signal = "SIGUSR2"
)

// Every signal a mount may name.
//
// A slice rather than a set so that the error naming the accepted signals lists them
// the same way each time.
var signals = []Signal{SignalHUP, SignalUSR1, SignalUSR2}

// Matches reports whether the reference names the port with the given name and
// number.
//
// Both forms are checked, since a reference is written as either. A port name may not
// read as a number, so at most one of the two can match.
func (p PortRef) Matches(name string, to int) bool {
	if p == "" {
		return false
	}

	if string(p) == name {
		return true
	}

	number, err := strconv.Atoi(string(p))

	return err == nil && number == to
}

// UnmarshalYAML decodes a port reference from the scalar it was written as.
//
// Written by hand because a reference is either a name or a number, and yaml.v3
// refuses to decode a number into a string. Without this a manifest naming the port
// itself would have to quote it, which nothing else in a manifest asks for.
func (p *PortRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("port must be a name or a port number, got %s", node.Tag)
	}

	*p = PortRef(node.Value)

	return nil
}

// KindOf reports which source mount names, which is determined by the field it
// carries rather than by a discriminator.
//
// Returns ErrNoMountSource when no source is named, or ErrAmbiguousMountSource when
// more than one is.
func KindOf(mount VolumeMount) (MountKind, error) {
	named := make([]MountKind, 0, 3)
	for _, candidate := range []struct {
		kind  MountKind
		value string
	}{
		{MountVolume, mount.Name},
		{MountSecret, mount.Secret},
		{MountVariable, mount.Var},
	} {
		if candidate.value != "" {
			named = append(named, candidate.kind)
		}
	}

	switch len(named) {
	case 0:
		return "", ErrNoMountSource
	case 1:
		return named[0], nil
	default:
		return "", fmt.Errorf("%w: %s", ErrAmbiguousMountSource, joinKinds(named))
	}
}

// Source returns the name of what the mount takes its contents from, whichever kind
// of source that is.
//
// This exists so that code which does not care where a mount points — reporting it,
// checking it for duplicates — does not have to ask which field to read.
func (m VolumeMount) Source() string {
	switch {
	case m.Secret != "":
		return m.Secret
	case m.Var != "":
		return m.Var
	default:
		return m.Name
	}
}

// Reference returns what the mount reads, reporting false for a mount of a volume.
//
// A mounted secret or variable is the same thing an env value references, so it
// resolves through the same identity rather than through a second notion of what a
// workload reads.
func (m VolumeMount) Reference() (Reference, bool) {
	switch {
	case m.Secret != "":
		return Reference{Kind: KindSecret, Name: m.Secret}, true
	case m.Var != "":
		return Reference{Kind: KindVariable, Name: m.Var}, true
	default:
		return Reference{}, false
	}
}

// joinKinds names several mount sources for an error reporting that a mount named
// more than one.
func joinKinds(kinds []MountKind) string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}

	return strings.Join(names, " and ")
}

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
			if mapping.Name != nil {
				port.Name = *mapping.Name
			}
			if mapping.From != nil {
				port.From = *mapping.From
			}
			if mapping.Protocol != nil {
				port.Protocol = Protocol(*mapping.Protocol)
			}

			port.defaults()

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
	out.Resources = newResources(spec.Resources)

	if spec.Container != nil {
		out.Container = &Container{Image: spec.Container.Image}

		if spec.Container.Pull != nil {
			out.Container.Pull = PullPolicy(*spec.Container.Pull)
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
		out.Exec = &Exec{Command: spec.Exec.Command}
	}

	return out
}

// NewVolumeMount maps a wire mount onto the canonical shape.
//
// Exported so that a caller which only cares about what a workload mounts can convert
// those alone. Converting the whole specification through NewSpec allocates a restart
// policy and the rest of it, which is waste on a path asked about every workload on
// every reconciliation pass.
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
	if mount.Secret != nil {
		out.Secret = *mount.Secret
	}
	if mount.Var != nil {
		out.Var = *mount.Var
	}
	if mount.Signal != nil {
		out.Signal = Signal(*mount.Signal)
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
		health.Port = PortRef(*spec.Port)
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

// newResources maps wire resource limits onto the canonical shape.
//
// The memory size is carried across as written rather than parsed here, so a value
// that does not parse survives to be reported by validation instead of erroring on
// the path a client uses to read a workload back.
func newResources(spec *api.ResourcesSpec) *Resources {
	if spec == nil {
		return nil
	}

	var resources Resources

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

// defaults fills in what a port left unset.
//
// A manifest naming no protocol asks for TCP, which is what a specification written
// before the protocol existed meant. Resolving it here rather than at every reader
// keeps a port's protocol something the rest of orca can rely on being set.
func (p *Port) defaults() {
	if p.Protocol == "" {
		p.Protocol = ProtocolTCP
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

// WireResources maps the canonical resource limits onto the wire format.
func WireResources(resources *Resources) *api.ResourcesSpec {
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
	if health.Port != "" {
		spec.Port = new(string(health.Port))
	}

	return &spec
}
