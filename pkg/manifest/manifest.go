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
	"net/url"
	"regexp"

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

	if err := Validate(spec); err != nil {
		return Spec{}, err
	}

	return spec, nil
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
		validation.Field(&spec.Schedule, validation.By(validCron)),
	)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	runtime, err := RuntimeOf(spec)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	switch runtime {
	case RuntimeContainer:
		return validateContainer(*spec.Container)
	case RuntimeScript:
		return validateScript(*spec.Script)
	default:
		return nil
	}
}

// RuntimeOf reports which runtime spec describes, which is determined by the block
// it carries rather than by a discriminator field.
//
// Returns ErrNoRuntime when no block is present, or ErrAmbiguousRuntime when more
// than one is.
func RuntimeOf(spec Spec) (Runtime, error) {
	switch {
	case spec.Container != nil && spec.Script != nil:
		return "", ErrAmbiguousRuntime
	case spec.Container != nil:
		return RuntimeContainer, nil
	case spec.Script != nil:
		return RuntimeScript, nil
	default:
		return "", ErrNoRuntime
	}
}

func validateContainer(spec Container) error {
	err := validation.ValidateStruct(&spec,
		validation.Field(&spec.Image, validation.Required),
		validation.Field(&spec.Ports, validation.By(validPorts)),
	)
	if err != nil {
		return fmt.Errorf("invalid container: %w", err)
	}

	return nil
}

func validateScript(spec Script) error {
	switch {
	case spec.Source != "" && spec.Raw != "":
		return errors.New("invalid script: only one of source or raw may be specified")
	case spec.Source == "" && spec.Raw == "":
		return errors.New("invalid script: one of source or raw is required")
	}

	if spec.Source == "" {
		return nil
	}

	if _, err := url.Parse(spec.Source); err != nil {
		return fmt.Errorf("invalid script: source must be a valid url: %w", err)
	}

	return nil
}

// validPorts checks that every published port is usable and that no two entries
// describe the same port, either inside the container or on the host.
func validPorts(value any) error {
	ports, ok := value.([]Port)
	if !ok || len(ports) == 0 {
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

func validCron(value any) error {
	schedule, ok := value.(string)
	if !ok || schedule == "" {
		return nil
	}

	// The standard five-field form, matching what an operator would put in a
	// crontab, rather than the seconds-resolution variant.
	if _, err := cron.ParseStandard(schedule); err != nil {
		return errors.New("must be a valid cron expression")
	}

	return nil
}
