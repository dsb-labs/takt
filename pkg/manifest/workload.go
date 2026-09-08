package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

var (
	// ErrNoRuntime is returned when a manifest describes no runtime at all.
	ErrNoRuntime = errors.New("no runtime specified")
	// ErrAmbiguousRuntime is returned when a manifest describes more than one
	// runtime, leaving no single driver to run it.
	ErrAmbiguousRuntime = errors.New("more than one runtime specified")
	// ErrNoMountSource is returned when a mount names nothing to mount.
	ErrNoMountSource = errors.New("no mount source specified")
	// ErrAmbiguousMountSource is returned when a mount names more than one thing to
	// mount, leaving no single source for what appears at the path.
	ErrAmbiguousMountSource = errors.New("more than one mount source specified")
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
		// takt keeps trying.
		Attempts int `json:"attempts,omitempty"`
		// How long to wait before the first restart, doubling on each consecutive
		// failure. Zero means DefaultRestartDelay.
		Delay time.Duration `json:"delay,omitempty"`
	}

	// The Spec type describes the desired state of a workload.
	//
	// It is the canonical shape of a workload throughout takt's public API: what
	// ParseWorkload produces from a manifest file, and what the client submits. Optional
	// values are plain zero values rather than pointers, so callers can build one
	// by hand without ceremony.
	//
	// The JSON tags are explicit on every field because this encoding is what takt
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
		// How many instances of the workload to run. Zero means one, which
		// Defaults resolves. Each instance publishes the workload's ports on
		// host ports of its own.
		Count int `json:"count"`
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
		// been through Defaults, which ParseWorkload and the wire mapping both call.
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
	// The timing fields mean the same thing for any runtime. The probe fields do not,
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
		// The pid namespace the container runs in, spelled the way docker spells
		// it. Only "host" is accepted, which shares the host's namespace the way
		// an exec workload always does. Empty runs in a namespace of its own.
		PidMode string `yaml:"pidMode" json:"pidMode,omitempty"`
		// The network the container joins, spelled the way docker spells it. Only
		// "host" is accepted, which shares the host's network namespace so the
		// container binds host ports directly. Empty runs on docker's default
		// bridge.
		//
		// A host-networked workload publishes ports the way an exec one does: the
		// port it binds is the host port, so every port's host side equals the
		// port inside and takt records it rather than allocating a mapping. This
		// is why such a workload cannot run more than one instance — two
		// containers cannot both bind one host port.
		NetworkMode string `yaml:"networkMode" json:"networkMode,omitempty"`
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
	// the path: a volume is a directory that outlives the workload, a secret or
	// a variable is a file holding what takt holds under that name, and a path is a
	// file or directory on the host that takt does not manage. The source is
	// derived from the field that is present rather than from a discriminator, as a
	// specification's runtime is.
	VolumeMount struct {
		// Where the volume's data is on the host, resolved by the server from the
		// named volume. Empty for a mounted secret or variable.
		//
		// Tagged out of the YAML encoding because it is not the operator's to write:
		// a manifest naming a path here is rejected as an unknown field. It is part
		// of the JSON encoding because the server stores it, as it stores an
		// allocated host port, so that the runtime is given a path rather than a
		// name to look up. Being stored, it is covered by the hash, so a volume whose
		// path changed replaces the instances bound to the old one.
		//
		// A mounted value carries none. Where the server writes the file is its own
		// layout and changes with every version of the workload, so storing it would
		// move the hash for a reason the operator did not ask for. A path mount
		// carries none either: the path field already says where the data is.
		From string `json:"from,omitempty" yaml:"-"`
		// The volume to mount, which must already exist.
		Name string `json:"name,omitempty"`
		// The secret to mount as a file, which must already exist.
		Secret string `json:"secret,omitempty"`
		// The variable to mount as a file, which must already exist.
		Var string `json:"var,omitempty"`
		// The host file or directory to mount, written as an absolute path.
		//
		// This is how a workload reaches data takt does not manage: a media
		// library on its own mount point, or the docker socket. A host path
		// reaches outside takt-managed state, so the server accepts one only
		// when its configuration allows the path. This package cannot check
		// that, because it validates manifests on machines that are not the
		// host.
		Path string `json:"path,omitempty"`
		// Where the workload finds what is mounted, written the same way whichever
		// runtime runs it. Where it resolves to differs, because a container has a
		// filesystem of its own and a process on the host does not.
		//
		// For a container it is the path inside the container. For an exec workload
		// what is mounted is placed at this path relative to the workload's working
		// directory, and such a workload reaches it by that relative path: confining
		// the process so the absolute one resolved there would need privileges takt
		// does not have.
		To string `json:"to"`
		// The signal to send the workload when the mounted value changes, rather than
		// replacing its instance. Empty replaces the instance, which is what a
		// workload that reads a file once wants.
		//
		// Only a mounted secret or variable may name one. A volume holds whatever the
		// workload puts there, so there is no change takt could report.
		Signal Signal `json:"signal,omitempty"`
		// Whether the workload may only read what is mounted. Applies to any
		// source, so a shared volume can be handed to a workload that should not
		// change it.
		//
		// Container workloads only. The exec runtime mounts through a symbolic
		// link, which cannot make anything read-only, so it rejects this rather
		// than ignoring it.
		//
		// The yaml tag is explicit for the reason Container.ReadOnly's is:
		// yaml.v3 matches against the lowercased Go field name, and the key is
		// spelled camelCase on the wire.
		ReadOnly bool `yaml:"readOnly" json:"readOnly,omitempty"`
		// How mount events travel between the host and the container, spelled
		// the way docker spells it. "rslave" makes a filesystem mounted on the
		// host after the workload starts visible inside, which is what a
		// workload observing the whole host wants. "rshared" also carries the
		// mounts the workload creates back to the host. Empty keeps docker's
		// default, which carries nothing in either direction.
		//
		// A host path only, on the container runtime only. Nothing is ever
		// mounted beneath a takt-managed volume, and the exec runtime mounts
		// through a symbolic link that cannot propagate anything.
		Propagation string `json:"propagation,omitempty"`
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
	// MountPath mounts a file or directory on the host that takt does not manage.
	MountPath MountKind = "path"
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
	named := make([]MountKind, 0, 4)

	if mount.Name != "" {
		named = append(named, MountVolume)
	}

	if mount.Secret != "" {
		named = append(named, MountSecret)
	}

	if mount.Var != "" {
		named = append(named, MountVariable)
	}

	if mount.Path != "" {
		named = append(named, MountPath)
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
	case m.Path != "":
		return m.Path
	default:
		return m.Name
	}
}

// Reference returns what the mount reads, reporting false for a mount of a volume
// or a host path.
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

// Defaults fills in what the specification left unset, so that validation checks
// what will actually be used rather than zeroes.
//
// Exported because it runs however a Spec was built, decoded from YAML by ParseWorkload or
// converted from the wire format by the caller that received one. A default applied
// on only one of those paths would make the same manifest behave differently
// depending on how it reached the server.
//
// Every workload has an answer to what happens when it ends, so a specification
// naming no policy still gets one.
func (s *Spec) Defaults() {
	if s.Count == 0 {
		s.Count = 1
	}

	if s.Restart == nil {
		s.Restart = new(Restart)
	}

	s.Restart.defaults()

	for i := range s.Ports {
		s.Ports[i].defaults()
	}

	// A host-networked container binds the host port directly, so the host side
	// equals the port inside. Derived here so the pinned port reaches the hash
	// and the claim the way an operator-written pin does, and so the rest of
	// takt sees one shape whether or not the operator wrote the host port.
	if s.Container != nil && s.Container.NetworkMode == "host" {
		for i := range s.Ports {
			if s.Ports[i].From == 0 {
				s.Ports[i].From = s.Ports[i].To
			}
		}
	}

	if s.Schedule != nil {
		s.Schedule.defaults()
	}

	if s.Health != nil {
		s.Health.defaults()
	}
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
// keeps a port's protocol something the rest of takt can rely on being set.
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
// Attempts are counted so that a workload can be told to give up. Zero means takt
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

// ParseWorkload reads a workload manifest from r and returns the specification it
// describes.
//
// The manifest is validated as it is parsed, so a specification returned from
// ParseWorkload is well-formed: it names a version this package understands, a usable
// name, exactly one runtime, and a schedule that parses as cron when present.
// Unknown fields are rejected rather than ignored, so a typo in a key is reported
// instead of silently doing nothing.
//
// YAML keys are matched against the lowercased Go field names of Spec. That holds
// while every manifest key is a single word, as they all are today. A multi-word
// field would need an explicit yaml tag, and the tests pin the current mapping.
func ParseWorkload(r io.Reader) (Spec, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Values left unset in the file are resolved before validation, so the rules check
	// what will actually be used rather than zeroes.
	spec.Defaults()

	if err := ValidateWorkload(spec); err != nil {
		return Spec{}, err
	}

	return spec, nil
}

// DecodeWorkload reads a stored specification, resolving the defaults as ParseWorkload does.
//
// A specification is stored with its defaults already resolved, so this normally
// changes nothing. It runs anyway because a decoded specification is not otherwise a
// resolved one: a caller reading a field the manifest left unset would find a zero
// value where every other path finds the default.
func DecodeWorkload(data []byte) (Spec, error) {
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	spec.Defaults()

	return spec, nil
}

// ValidateWorkload reports whether spec is a usable workload specification.
func ValidateWorkload(spec Spec) error {
	if err := validateHeader(spec.Version, spec.Name); err != nil {
		return err
	}

	if err := validateCount(spec); err != nil {
		return err
	}

	if err := validateRestart(spec.Restart); err != nil {
		return err
	}

	if err := validateSchedule(spec); err != nil {
		return err
	}

	if err := ValidateLabels(spec.Labels); err != nil {
		return err
	}

	runtime, err := RuntimeOf(spec)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	if err = validatePorts(spec, runtime); err != nil {
		return err
	}

	if err = validateVolumes(spec.Volumes, runtime); err != nil {
		return err
	}

	if err = validateEnv(spec); err != nil {
		return err
	}

	if err = validateHealth(spec, runtime); err != nil {
		return err
	}

	if err = validateResources(spec, runtime); err != nil {
		return err
	}

	switch runtime {
	case RuntimeContainer:
		return validateContainer(*spec.Container)
	case RuntimeExec:
		return validateExec(*spec.Exec)
	default:
		return nil
	}
}

// validateRestart reports whether the workload's restart policy is usable.
func validateRestart(restart *Restart) error {
	if restart == nil {
		return nil
	}

	switch {
	case restart.Policy != RestartAlways && restart.Policy != RestartOnFailure && restart.Policy != RestartNever:
		return fmt.Errorf("invalid restart: policy must be %q, %q or %q",
			RestartAlways, RestartOnFailure, RestartNever)
	case restart.Attempts < 0:
		return errors.New("invalid restart: attempts must not be negative")
	case restart.Delay <= 0:
		return errors.New("invalid restart: delay must be greater than zero")
	}

	return nil
}

// validateCount reports whether the workload's instance count is one takt can run.
func validateCount(spec Spec) error {
	if spec.Count < 1 {
		return errors.New("invalid count: must be at least 1")
	}

	return nil
}

// validateSchedule reports whether the workload's schedule is one takt can act on.
func validateSchedule(spec Spec) error {
	schedule := spec.Schedule
	if schedule == nil {
		return nil
	}

	if schedule.Cron == "" {
		return errors.New("invalid schedule: cron is required")
	}

	if _, err := cron.ParseStandard(schedule.Cron); err != nil {
		return errors.New("invalid schedule: cron must be a valid expression")
	}

	if schedule.Overlap != OverlapReplace && schedule.Overlap != OverlapSkip {
		return fmt.Errorf("invalid schedule: overlap must be %q or %q", OverlapReplace, OverlapSkip)
	}

	// A check restarts a workload that stops answering, and a scheduled workload is
	// expected to end. Together they would have the check fighting the schedule, so
	// the pair is rejected rather than left to whichever acts first.
	if spec.Health != nil {
		return errors.New("invalid schedule: a scheduled workload cannot declare a health check")
	}

	// N copies of a cron job firing at once is almost never what a schedule means,
	// so the pair is rejected rather than guessed at.
	if spec.Count > 1 {
		return errors.New("invalid schedule: a scheduled workload cannot run more than one instance")
	}

	return nil
}

// validatePorts reports whether the workload's ports are ones its runtime can publish.
//
// A runtime with nothing to publish rejects them rather than ignoring them, for the
// same reason a check it cannot perform is rejected: a workload whose ports never
// reach anything looks like takt failing rather than the manifest being wrong.
func validatePorts(spec Spec, runtime Runtime) error {
	if len(spec.Ports) == 0 {
		return nil
	}

	if err := validPorts(spec.Ports); err != nil {
		return err
	}

	// A host-networked workload binds the host ports directly, so it is the exec
	// case in a container: two instances cannot both bind one host port. Caught
	// before the pinned check below so the message names the reason rather than
	// the derived pin.
	hostNet := spec.Container != nil && spec.Container.NetworkMode == "host"
	if hostNet && spec.Count > 1 {
		return errors.New("invalid ports: a host-networked workload cannot run more than one instance, " +
			"because its ports bind the host directly")
	}

	// One host port reaches one listener, so a pinned port cannot serve more than
	// one instance. Exec workloads must pin every port, which is why a count above
	// one is a container-only feature for a workload that publishes anything.
	if spec.Count > 1 {
		for _, port := range spec.Ports {
			if port.From != 0 {
				return fmt.Errorf("invalid ports: port %d pins host port %d, "+
					"which cannot reach more than one instance", port.To, port.From)
			}
		}
	}

	switch runtime {
	case RuntimeContainer:
		if hostNet {
			// Defaults derived the host side from the port inside, so every port
			// must now agree with it. An explicit host port that differs is asking
			// for a mapping host networking cannot make.
			for _, port := range spec.Ports {
				if port.From != port.To {
					return fmt.Errorf("invalid ports: port %d cannot pin host port %d in a "+
						"host-networked workload, which binds the port inside directly", port.To, port.From)
				}
			}

			return nil
		}

		// A container listens inside its own namespace, so the host port is a mapping
		// the server is free to choose.
		return nil
	case RuntimeExec:
		// An exec process binds a host port itself, so there is no mapping to make and
		// nothing for the server to choose. It records the port so that no other
		// workload is given it, and the process is told which one by whoever wrote the
		// command.
		for _, port := range spec.Ports {
			if port.From == 0 {
				return fmt.Errorf("invalid ports: port %d/%s must name the host port it binds, "+
					"which the %s runtime does not allocate", port.To, port.Protocol, runtime)
			}
		}

		return nil
	default:
		return fmt.Errorf("invalid ports: the %s runtime cannot publish ports", runtime)
	}
}

// validateResources reports whether the workload's resource limits are ones its
// runtime can enforce.
//
// A runtime that cannot enforce them rejects them rather than ignoring them, for the
// same reason a port it cannot publish is rejected: a limit that never applies looks
// like takt failing rather than the manifest being wrong.
func validateResources(spec Spec, runtime Runtime) error {
	resources := spec.Resources
	if resources == nil {
		return nil
	}

	if resources.Memory == "" && resources.CPU == 0 && resources.Pids == 0 {
		return errors.New("invalid resources: at least one of memory, cpu or pids is required")
	}

	if resources.Memory != "" {
		if size, err := units.RAMInBytes(resources.Memory); err != nil || size <= 0 {
			return fmt.Errorf("invalid resources: %q is not a memory size", resources.Memory)
		}
	}

	switch {
	case resources.CPU < 0:
		return errors.New("invalid resources: cpu must not be negative")
	case resources.Pids < 0:
		return errors.New("invalid resources: pids must not be negative")
	}

	switch runtime {
	case RuntimeContainer, RuntimeExec:
		// A container already runs in cgroups of its own, and the exec runtime
		// puts a limited process in one. Whether the host lets it — enforcing on
		// a host process takes a delegated cgroup subtree — is the server's to
		// answer, because this package validates manifests on machines that are
		// not the host.
		return nil
	default:
		// A runtime that cannot enforce the limits rejects them rather than
		// accepting them and silently never applying them.
		return fmt.Errorf("invalid resources: the %s runtime cannot enforce limits", runtime)
	}
}

// validateVolumes reports whether the workload's mounts are ones takt can honour.
//
// The rules are the same for either runtime, which is the point of the field: a
// workload moved between them keeps its manifest, and nobody has to remember which
// runtime wants which spelling. Where the path resolves to differs — a container has
// a filesystem of its own, an exec workload has its working directory — but what a
// manifest may say does not.
//
// The one exception is a read-only mount, which is why the runtime is passed. The
// exec runtime mounts through a symbolic link, which cannot make anything
// read-only, so it rejects the field rather than ignoring it — for the reason a
// port it cannot publish is rejected: a promise that never applies looks like takt
// failing rather than the manifest being wrong.
//
// A mount names exactly one source, and the rules that follow from the source are
// checked against the kind rather than against whichever field happened to be set: a
// volume takes no signal, and a value mount is a file rather than a directory.
func validateVolumes(mounts []VolumeMount, runtime Runtime) error {
	if len(mounts) == 0 {
		return nil
	}

	// Keyed by kind and name together, so that a secret and a variable sharing a name
	// are two mounts rather than a duplicate. They resolve from different places, as
	// the two kinds of reference do.
	sources := make(map[Reference]struct{}, len(mounts))
	paths := make(map[string]struct{}, len(mounts))

	for _, mount := range mounts {
		kind, err := KindOf(mount)
		if err != nil {
			return fmt.Errorf("invalid volumes: %w", err)
		}

		source := mount.Source()

		if kind == MountPath {
			if !path.IsAbs(source) {
				return fmt.Errorf("invalid volumes: path %q must be absolute", source)
			}

			// Cleaned before it is used as a key, so that "/data" and "/data/" are
			// recognised as the same source rather than as two.
			source = path.Clean(source)
		} else if !namePattern.MatchString(source) || len(source) > 63 {
			return fmt.Errorf("invalid volumes: %q is not a %s name: must be lowercase "+
				"alphanumeric, optionally separated by dashes", source, kind)
		}

		if err = validateMountSignal(mount, kind); err != nil {
			return err
		}

		if mount.ReadOnly && runtime == RuntimeExec {
			return fmt.Errorf("invalid volumes: %s %q cannot be read-only, because the %s "+
				"runtime mounts through a symbolic link and cannot enforce it", kind, source, runtime)
		}

		if err = validatePropagation(mount, kind, runtime); err != nil {
			return err
		}

		key := Reference{Kind: ReferenceKind(kind), Name: source}
		if _, ok := sources[key]; ok {
			return fmt.Errorf("invalid volumes: %s %q is mounted more than once", kind, source)
		}
		sources[key] = struct{}{}

		if !path.IsAbs(mount.To) {
			return fmt.Errorf("invalid volumes: %s %q must name an absolute path to mount at, got %q",
				kind, source, mount.To)
		}

		// Cleaned before comparing, so that "/data" and "/data/" are recognised as
		// the same mount rather than as two.
		to := path.Clean(mount.To)

		// A working directory is not a volume, and for a container this would be the
		// whole filesystem.
		if to == "/" {
			return fmt.Errorf("invalid volumes: %s %q cannot be mounted at %q", kind, source, mount.To)
		}

		if _, ok := paths[to]; ok {
			return fmt.Errorf("invalid volumes: %q is mounted more than once", to)
		}
		paths[to] = struct{}{}
	}

	return nil
}

// validatePropagation reports whether a mount's propagation is one takt can
// honour: a docker spelling it accepts, on a host path, on the container
// runtime.
//
// Only the recursive values are accepted. Nobody has named a use for the
// non-recursive pair, and the private pair is docker's default, which an
// empty field already says.
func validatePropagation(mount VolumeMount, kind MountKind, runtime Runtime) error {
	if mount.Propagation == "" {
		return nil
	}

	if mount.Propagation != "rslave" && mount.Propagation != "rshared" {
		return fmt.Errorf("invalid volumes: propagation must be %q, %q or absent, got %q",
			"rslave", "rshared", mount.Propagation)
	}

	if kind != MountPath {
		return fmt.Errorf("invalid volumes: %s %q cannot name a propagation, because "+
			"nothing is ever mounted beneath a %s", kind, mount.Source(), kind)
	}

	if runtime == RuntimeExec {
		return fmt.Errorf("invalid volumes: path %q cannot name a propagation, because the %s "+
			"runtime mounts through a symbolic link and cannot propagate anything",
			mount.Source(), runtime)
	}

	return nil
}

// validateMountSignal reports whether the signal a mount names is one takt will send
// for a mount of that kind.
//
// A volume or a host path takes none at all. takt does not know what changes inside
// either, so there is no change it could report — and a manifest naming a signal
// there is asking for something that would never happen, which is worth saying
// rather than ignoring.
func validateMountSignal(mount VolumeMount, kind MountKind) error {
	if mount.Signal == "" {
		return nil
	}

	if kind == MountVolume || kind == MountPath {
		return fmt.Errorf("invalid volumes: %s %q cannot name a signal, because takt does not "+
			"know when its contents change", kind, mount.Source())
	}

	if !slices.Contains(signals, mount.Signal) {
		return fmt.Errorf("invalid volumes: %s %q names signal %q, which must be one of %s",
			kind, mount.Source(), mount.Signal, acceptedSignals())
	}

	return nil
}

// acceptedSignals names every signal a mount may ask for, for an error reporting one
// that is not among them.
func acceptedSignals() string {
	names := make([]string, 0, len(signals))
	for _, signal := range signals {
		names = append(names, string(signal))
	}

	return strings.Join(names, ", ")
}

// validateHealth reports whether the workload's health check is one its runtime can
// actually perform.
//
// The timing fields apply to any runtime, so they are checked for every workload. The
// probe fields do not: a check is performed against an address, and a runtime with
// nothing to address cannot be probed. Rejecting that combination matters more than
// ignoring it would — a workload whose check can never run would sit reported as
// starting forever, which looks like takt failing rather than the manifest being
// wrong.
func validateHealth(spec Spec, runtime Runtime) error {
	health := spec.Health
	if health == nil {
		return nil
	}

	switch {
	case health.Interval <= 0:
		return errors.New("invalid health: interval must be greater than zero")
	case health.Timeout <= 0:
		return errors.New("invalid health: timeout must be greater than zero")
	case health.Timeout > health.Interval:
		// A check that may run longer than the gap between checks would overlap
		// itself, so the failure count would stop meaning consecutive failures.
		return errors.New("invalid health: timeout must not exceed interval")
	case health.Retries < 1:
		return errors.New("invalid health: retries must be at least one")
	case health.StartPeriod < 0:
		return errors.New("invalid health: start period must not be negative")
	}

	switch {
	case health.HTTP != "" && health.TCP:
		return errors.New("invalid health: only one of http or tcp may be specified")
	case health.HTTP == "" && !health.TCP:
		return errors.New("invalid health: one of http or tcp is required")
	}

	// A check is performed against an address, so the question is whether the workload
	// publishes one rather than which runtime it is. Any runtime that publishes a port
	// can be probed at it, and one that publishes nothing cannot be probed at all.
	if len(spec.Ports) == 0 {
		return fmt.Errorf("invalid health: the workload publishes no ports to check, "+
			"so the %s runtime cannot be probed", runtime)
	}

	return validateHealthPort(*health, spec.Ports)
}

// validateHealthPort reports whether the check names a port the workload actually
// publishes, since a probe is performed against a published address.
//
// Only a TCP port counts. Both probes connect, and a connection to a UDP port always
// succeeds whatever is behind it, so a check against one would report every workload
// as healthy. A workload publishing UDP alone is refused a check for the same reason
// a runtime that cannot be probed is.
func validateHealthPort(health Health, ports []Port) error {
	checkable := slices.DeleteFunc(slices.Clone(ports), func(port Port) bool {
		return port.Protocol != ProtocolTCP
	})

	if len(checkable) == 0 {
		return fmt.Errorf("invalid health: the workload publishes no %s port to check, "+
			"and a %s port always accepts a connection", ProtocolTCP, ProtocolUDP)
	}

	if health.Port == "" {
		if len(checkable) > 1 {
			return errors.New("invalid health: port is required when more than one port is published")
		}

		return nil
	}

	if !slices.ContainsFunc(checkable, func(port Port) bool { return health.Port.Matches(port.Name, port.To) }) {
		return fmt.Errorf("invalid health: port %q is not published by the workload over %s",
			health.Port, ProtocolTCP)
	}

	return nil
}

// RuntimeOf reports which runtime spec describes, which is determined by the block
// it carries rather than by a discriminator field.
//
// Returns ErrNoRuntime when no block is present, or ErrAmbiguousRuntime when more
// than one is.
func RuntimeOf(spec Spec) (Runtime, error) {
	switch {
	case spec.Container != nil && spec.Exec != nil:
		return "", ErrAmbiguousRuntime
	case spec.Container != nil:
		return RuntimeContainer, nil
	case spec.Exec != nil:
		return RuntimeExec, nil
	default:
		return "", ErrNoRuntime
	}
}

func validateContainer(spec Container) error {
	if spec.Image == "" {
		return errors.New("invalid container: image is required")
	}

	if err := validCommand(spec.Command); err != nil {
		return fmt.Errorf("invalid container: %w", err)
	}

	// Empty is accepted and means PullMissing. It is not resolved to it here, so that
	// a manifest which says nothing encodes nothing on the wire.
	switch spec.Pull {
	case "", PullAlways, PullMissing, PullNever:
	default:
		return fmt.Errorf("invalid container: pull must be %q, %q or %q",
			PullAlways, PullMissing, PullNever)
	}

	// The user and the capability names are not held to a pattern. Docker accepts
	// several spellings of a user and its capability set changes between versions,
	// so a pattern here would reject forms the runtime is happy with. An empty
	// element is still rejected, the way an empty command element is: it reaches
	// the runtime as a capability that means nothing or was not written.
	for _, capabilities := range [][]string{spec.CapAdd, spec.CapDrop} {
		for i, capability := range capabilities {
			if strings.TrimSpace(capability) == "" {
				return fmt.Errorf("invalid container: capability element %d is empty", i)
			}
		}
	}

	// Docker also accepts "container:<id>", which names a container takt did not
	// start and cannot promise anything about, so only the host namespace is
	// accepted here.
	if spec.PidMode != "" && spec.PidMode != "host" {
		return errors.New(`invalid container: pidMode must be "host" or absent`)
	}

	// Docker also accepts "bridge", "none" and "container:<id>", none of which
	// takt has a use for: the bridge is the default an empty field already
	// names, and the others make ports mean something takt does not model.
	if spec.NetworkMode != "" && spec.NetworkMode != "host" {
		return errors.New(`invalid container: networkMode must be "host" or absent`)
	}

	return nil
}

// validCommand reports whether every element of a command is something a runtime can
// execute.
//
// An empty element is rejected rather than dropped. It reaches the runtime as an empty
// argument, which either means nothing or means something the operator did not write,
// and a manifest that says nothing about it is easier to fix than a container that
// behaves oddly.
func validCommand(command []string) error {
	if len(command) == 0 {
		return nil
	}

	for i, part := range command {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("command element %d is empty", i)
		}
	}

	return nil
}

func validateExec(spec Exec) error {
	if len(spec.Command) == 0 {
		return errors.New("invalid exec: command is required")
	}

	if err := validCommand(spec.Command); err != nil {
		return fmt.Errorf("invalid exec: %w", err)
	}

	return nil
}

// validPorts checks that every published port is usable and that no two entries
// describe the same port, either inside the workload or on the host.
//
// A port's identity includes its protocol, because TCP and UDP are separate address
// spaces. 53/tcp beside 53/udp is one workload publishing two different ports, which
// is what DNS wants, where 53/tcp twice is the same port named twice.
func validPorts(ports []Port) error {
	if len(ports) == 0 {
		return nil
	}

	type key struct {
		port     int
		protocol Protocol
	}

	seenTo := make(map[key]struct{}, len(ports))
	seenFrom := make(map[key]struct{}, len(ports))
	named := make(map[string]int, len(ports))

	for _, port := range ports {
		if err := validProtocol(port.Protocol); err != nil {
			return err
		}

		if err := validPortName(port, named); err != nil {
			return err
		}

		if err := validPort(port.To, "to"); err != nil {
			return err
		}

		if _, ok := seenTo[key{port.To, port.Protocol}]; ok {
			return fmt.Errorf("port %d/%s is published more than once", port.To, port.Protocol)
		}
		seenTo[key{port.To, port.Protocol}] = struct{}{}

		// An unset host port asks for an allocation, so there is nothing to check
		// and no duplicate to find: each allocation is distinct by construction.
		if port.From == 0 {
			continue
		}

		if err := validPort(port.From, "from"); err != nil {
			return err
		}

		if _, ok := seenFrom[key{port.From, port.Protocol}]; ok {
			return fmt.Errorf("host port %d/%s is used more than once", port.From, port.Protocol)
		}
		seenFrom[key{port.From, port.Protocol}] = struct{}{}
	}

	return nil
}

// validPortName checks the name a port was given against the names already taken,
// recording it as taken when it holds.
//
// A name is held to the rules a workload name is, with one addition: it may not read
// as a number. A health check and a workload reference both accept either a name or
// the port itself, so a port called "8080" would be two different things written the
// same way.
//
// Two entries may share a name only when they publish the same port. That is one
// service published over TCP and UDP, which an operator names once and selects by
// that name whichever protocol they meant. Two different ports sharing a name would
// leave the name pointing at neither.
func validPortName(port Port, named map[string]int) error {
	if port.Name == "" {
		return nil
	}

	switch {
	case !namePattern.MatchString(port.Name) || len(port.Name) > maxLabelKeyLength:
		return fmt.Errorf("port name %q must be lowercase alphanumeric, optionally separated by dashes, "+
			"up to %d characters", port.Name, maxLabelKeyLength)
	case isNumber(port.Name):
		return fmt.Errorf("port name %q must not be a number, since a port is also named by the port itself",
			port.Name)
	}

	if to, ok := named[port.Name]; ok && to != port.To {
		return fmt.Errorf("port name %q is used by both %d and %d, so it names neither", port.Name, to, port.To)
	}

	named[port.Name] = port.To

	return nil
}

// isNumber reports whether a name reads as a port number rather than as a name.
func isNumber(name string) bool {
	_, err := strconv.Atoi(name)

	return err == nil
}
