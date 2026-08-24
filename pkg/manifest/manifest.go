// Package manifest provides parsing and validation of orca workload manifests.
//
// A manifest is the YAML file an operator writes to describe a workload. Parsing
// it is a client-side concern: it produces a Spec, which is what the client
// submits, so the OpenAPI document remains the only description of the wire format
// and the server never has to understand YAML.
package manifest

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

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
// while every manifest key is a single word, as they all are today; a multi-word
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
	//
	// Every workload has an answer to what happens when it ends, so a manifest naming
	// no policy still gets one.
	if spec.Restart == nil {
		spec.Restart = new(Restart)
	}

	spec.Restart.defaults()

	if spec.Schedule != nil {
		spec.Schedule.defaults()
	}

	if spec.Health != nil {
		spec.Health.defaults()
	}

	if err := Validate(spec); err != nil {
		return Spec{}, err
	}

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

	return nil
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

	if err = validateRestart(spec.Restart); err != nil {
		return err
	}

	if err = validateSchedule(spec); err != nil {
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

	if restart.invalidDelay != "" {
		return fmt.Errorf("invalid restart: %q is not a duration", restart.invalidDelay)
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
				return fmt.Errorf("invalid ports: port %d must name the host port it binds, "+
					"which the %s runtime does not allocate", port.To, runtime)
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
	case RuntimeContainer:
		// A container already runs in cgroups of its own, so the limits are ones
		// its runtime enforces as the container is created.
		return nil
	default:
		// A host process would need cgroup privileges orca has not got, so the
		// limits are rejected rather than accepted and silently never applied.
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

	if len(health.invalid) > 0 {
		return fmt.Errorf("invalid health: %s is not a duration", strings.Join(health.invalid, ", "))
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
func validateHealthPort(health Health, ports []Port) error {
	if len(ports) == 0 {
		return errors.New("invalid health: the workload publishes no ports to check")
	}

	if health.Port == 0 {
		if len(ports) > 1 {
			return errors.New("invalid health: port is required when more than one port is published")
		}

		return nil
	}

	if !slices.ContainsFunc(ports, func(port Port) bool { return port.To == health.Port }) {
		return fmt.Errorf("invalid health: port %d is not published by the workload", health.Port)
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
func validPorts(ports []Port) error {
	if len(ports) == 0 {
		return nil
	}

	seenTo := make(map[int]struct{}, len(ports))
	seenFrom := make(map[int]struct{}, len(ports))

	for _, port := range ports {
		if err := validPort(port.To, "to"); err != nil {
			return err
		}

		if _, ok := seenTo[port.To]; ok {
			return fmt.Errorf("port %d is published more than once", port.To)
		}
		seenTo[port.To] = struct{}{}

		// An unset host port asks for an allocation, so there is nothing to check
		// and no duplicate to find: each allocation is distinct by construction.
		if port.From == 0 {
			continue
		}

		if err := validPort(port.From, "from"); err != nil {
			return err
		}

		if _, ok := seenFrom[port.From]; ok {
			return fmt.Errorf("host port %d is used more than once", port.From)
		}
		seenFrom[port.From] = struct{}{}
	}

	return nil
}

func validPort(port int, field string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535, got %d", field, port)
	}

	return nil
}
