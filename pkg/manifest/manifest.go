// Package manifest provides parsing and validation of orca workload manifests.
//
// A manifest is the YAML file an operator writes to describe a workload. Parsing
// it is a client-side concern: it produces a Spec, which is what the client
// submits, so the OpenAPI document remains the only description of the wire format
// and the server never has to understand YAML.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/docker/go-units"
	validation "github.com/go-ozzo/ozzo-validation/v4"
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

// The manifest schema version this package understands.
const version = "v1"

// Names identify a workload in URLs and in the runtime's own namespace, so they
// are held to the DNS label rules that every runtime can represent.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Label keys follow the convention operators arrive with — app.kubernetes.io/name —
// rather than the stricter workload name pattern: lowercase alphanumeric at both
// ends, with dots, dashes, underscores and slashes between.
var labelKeyPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?$`)

const (
	// maxLabels caps how many labels one workload may carry. Thirty-two is more
	// than any sane manifest needs and small enough that the stored labels the
	// list query filter scans stay bounded.
	maxLabels = 32
	// maxLabelKeyLength is the cap every other name in orca is held to.
	maxLabelKeyLength = 63
	// maxLabelValueLength is counted in bytes, because the limit protects what
	// stores and displays the value rather than how many characters it reads
	// as. Generous enough for a URL or a one-line description.
	maxLabelValueLength = 256
	// reservedLabelPrefix marks the keys the docker driver writes for itself.
	reservedLabelPrefix = "orca."
)

// Parse reads a workload manifest from r and returns the specification it
// describes.
//
// The manifest is validated as it is parsed, so a specification returned from
// Parse is well-formed: it names a version this package understands, a usable
// name, exactly one runtime, and a schedule that parses as cron when present.
// Unknown fields are rejected rather than ignored, so a typo in a key is reported
// instead of silently doing nothing.
//
// YAML keys are matched against the lowercased Go field names of Spec. That holds
// while every manifest key is a single word, as they all are today. A multi-word
// field would need an explicit yaml tag, and the tests pin the current mapping.
func Parse(r io.Reader) (Spec, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Values left unset in the file are resolved before validation, so the rules check
	// what will actually be used rather than zeroes.
	spec.Defaults()

	if err := Validate(spec); err != nil {
		return Spec{}, err
	}

	return spec, nil
}

// Decode reads a stored specification, resolving the defaults as Parse does.
//
// A specification is stored with its defaults already resolved, so this normally
// changes nothing. It runs anyway because a decoded specification is not otherwise a
// resolved one: a caller reading a field the manifest left unset would find a zero
// value where every other path finds the default.
func Decode(data []byte) (Spec, error) {
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("failed to decode workload spec: %w", err)
	}

	spec.Defaults()

	return spec, nil
}

// ParseVolume reads a volume manifest from r and returns the volume it describes.
//
// A volume manifest carries no kind field. Which resource a file describes is decided
// by what it is given to, so a file naming a workload's fields is reported as having
// unknown keys rather than being accepted as half a volume.
func ParseVolume(r io.Reader) (Volume, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var volume Volume
	if err := decoder.Decode(&volume); err != nil {
		return Volume{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	if err := ValidateVolume(volume); err != nil {
		return Volume{}, err
	}

	return volume, nil
}

// ValidateVolume reports whether volume is a usable volume specification.
func ValidateVolume(volume Volume) error {
	err := validation.ValidateStruct(&volume,
		validation.Field(&volume.Version,
			validation.Required,
			validation.In(version).Error(fmt.Sprintf("must be %q", version)),
		),
		validation.Field(&volume.Name,
			validation.Required,
			validation.Length(1, 63),
			validation.Match(namePattern).Error("must be lowercase alphanumeric, optionally separated by dashes"),
		),
	)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	return ValidateLabels(volume.Labels)
}

// Validate reports whether spec is a usable workload specification.
func Validate(spec Spec) error {
	err := validation.ValidateStruct(&spec,
		validation.Field(&spec.Version,
			validation.Required,
			validation.In(version).Error(fmt.Sprintf("must be %q", version)),
		),
		validation.Field(&spec.Name,
			validation.Required,
			validation.Length(1, 63),
			validation.Match(namePattern).Error("must be lowercase alphanumeric, optionally separated by dashes"),
		),
	)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	if err = validateCount(spec); err != nil {
		return err
	}

	if err = validateRestart(spec.Restart); err != nil {
		return err
	}

	if err = validateSchedule(spec); err != nil {
		return err
	}

	if err = ValidateLabels(spec.Labels); err != nil {
		return err
	}

	runtime, err := RuntimeOf(spec)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	if err = validatePorts(spec, runtime); err != nil {
		return err
	}

	if err = validateVolumes(spec.Volumes); err != nil {
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

// validateCount reports whether the workload's instance count is one orca can run.
func validateCount(spec Spec) error {
	if spec.Count < 1 {
		return errors.New("invalid count: must be at least 1")
	}

	return nil
}

// validateSchedule reports whether the workload's schedule is one orca can act on.
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

// ValidateLabels reports whether the labels are ones orca will attach.
//
// Keys are held to the shape operators arrive with rather than to the workload
// name pattern, so app.kubernetes.io/name passes. Values are freer still — any
// printable text, since a label is read by people and compared as text by the
// list query filter — but control characters are refused because the value
// reaches container metadata and terminal output.
//
// The orca. prefix is refused for feedback rather than safety. The docker driver
// writes its own labels after copying these, so a spoofed key could never stick —
// but silently overwriting an operator's value is worse than telling them no.
//
// Exported because a secret and a variable carry labels too, and neither arrives
// through a manifest. Two answers to what a label may be would be worse than one
// answer in a package the other one has to import.
func ValidateLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return fmt.Errorf("invalid labels: %d labels exceeds the maximum of %d", len(labels), maxLabels)
	}

	// Keys are visited in sorted order so a manifest with several bad labels
	// reports the same one every time. The errors name only the key: a value
	// can be anything up to the request body limit, so it is never echoed.
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		if strings.HasPrefix(key, reservedLabelPrefix) {
			return fmt.Errorf("invalid labels: key %q uses the %q prefix, which is reserved for the labels orca writes itself",
				key, reservedLabelPrefix)
		}

		if !labelKeyPattern.MatchString(key) || len(key) > maxLabelKeyLength {
			return fmt.Errorf("invalid labels: key %q must be lowercase alphanumeric, optionally separated by "+
				"dots, dashes, underscores or slashes, up to %d characters", key, maxLabelKeyLength)
		}

		value := labels[key]
		if !utf8.ValidString(value) {
			return fmt.Errorf("invalid labels: the value of %q is not valid UTF-8", key)
		}

		if strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("invalid labels: the value of %q contains a control character", key)
		}

		if len(value) > maxLabelValueLength {
			return fmt.Errorf("invalid labels: the value of %q is %d bytes, which exceeds the maximum of %d",
				key, len(value), maxLabelValueLength)
		}
	}

	return nil
}

// validatePorts reports whether the workload's ports are ones its runtime can publish.
//
// A runtime with nothing to publish rejects them rather than ignoring them, for the
// same reason a check it cannot perform is rejected: a workload whose ports never
// reach anything looks like orca failing rather than the manifest being wrong.
func validatePorts(spec Spec, runtime Runtime) error {
	if len(spec.Ports) == 0 {
		return nil
	}

	if err := validPorts(spec.Ports); err != nil {
		return err
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
// like orca failing rather than the manifest being wrong.
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

// validateVolumes reports whether the workload's mounts are ones orca can honour.
//
// The rules are the same for either runtime, which is the point of the field: a
// workload moved between them keeps its manifest, and nobody has to remember which
// runtime wants which spelling. Where the path resolves to differs — a container has
// a filesystem of its own, an exec workload has its working directory — but what a
// manifest may say does not.
//
// A mount names exactly one source, and the rules that follow from the source are
// checked against the kind rather than against whichever field happened to be set: a
// volume takes no signal, and a value mount is a file rather than a directory.
func validateVolumes(mounts []VolumeMount) error {
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
		if !namePattern.MatchString(source) || len(source) > 63 {
			return fmt.Errorf("invalid volumes: %q is not a %s name: must be lowercase "+
				"alphanumeric, optionally separated by dashes", source, kind)
		}

		if err = validateMountSignal(mount, kind); err != nil {
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

// validateMountSignal reports whether the signal a mount names is one orca will send
// for a mount of that kind.
//
// A volume takes none at all. orca does not know what a workload writes into a volume,
// so there is no change it could report — and a manifest naming a signal there is
// asking for something that would never happen, which is worth saying rather than
// ignoring.
func validateMountSignal(mount VolumeMount, kind MountKind) error {
	if mount.Signal == "" {
		return nil
	}

	if kind == MountVolume {
		return fmt.Errorf("invalid volumes: volume %q cannot name a signal, because orca does not "+
			"know when its contents change", mount.Name)
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
// starting forever, which looks like orca failing rather than the manifest being
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
	err := validation.ValidateStruct(&spec,
		validation.Field(&spec.Image, validation.Required),
		validation.Field(&spec.Command, validation.By(validCommand)),
	)
	if err != nil {
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

	return nil
}

// validCommand reports whether every element of a command is something a runtime can
// execute.
//
// An empty element is rejected rather than dropped. It reaches the runtime as an empty
// argument, which either means nothing or means something the operator did not write,
// and a manifest that says nothing about it is easier to fix than a container that
// behaves oddly.
func validCommand(value any) error {
	command, ok := value.([]string)
	if !ok || len(command) == 0 {
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

// validProtocol reports whether a port names a protocol orca can publish it on.
func validProtocol(protocol Protocol) error {
	switch protocol {
	case ProtocolTCP, ProtocolUDP:
		return nil
	default:
		return fmt.Errorf("protocol %q is not one of %s or %s", protocol, ProtocolTCP, ProtocolUDP)
	}
}

func validPort(port int, field string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535, got %d", field, port)
	}

	return nil
}
