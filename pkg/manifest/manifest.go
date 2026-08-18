// Package manifest provides parsing and validation of orca workload manifests.
//
// A manifest is the YAML file an operator writes to describe a workload. It is a
// client-side authoring convenience: parsing produces the same specification type
// the API accepts, so the OpenAPI document remains the only description of the
// wire format and the server never has to understand YAML.
package manifest

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"

	"github.com/docker/go-connections/nat"
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"

	"github.com/dsb-labs/orca/internal/generated/api"
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
// The generated specification types carry only JSON tags, and YAML keys are
// matched against the lowercased Go field names. That holds while every manifest
// key is a single word, as they all are today. A multi-word field added to the
// specification would be spelled camelCase in JSON but have to be written
// lowercased here, so such a field needs an explicit yaml tag on the generated
// type — see the manifest tests, which assert the current mapping.
func Parse(r io.Reader) (api.WorkloadSpec, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var spec api.WorkloadSpec
	if err := decoder.Decode(&spec); err != nil {
		return api.WorkloadSpec{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	if err := Validate(spec); err != nil {
		return api.WorkloadSpec{}, err
	}

	return spec, nil
}

// Validate reports whether spec is a usable workload specification.
func Validate(spec api.WorkloadSpec) error {
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

	runtime, err := Runtime(spec)
	if err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}

	// The runtime blocks are generated types with no validation of their own, so
	// each is checked here against the rules its driver needs.
	switch runtime {
	case api.Container:
		return validateContainer(*spec.Container)
	case api.Script:
		return validateScript(*spec.Script)
	default:
		return nil
	}
}

func validateContainer(spec api.ContainerSpec) error {
	err := validation.ValidateStruct(&spec,
		validation.Field(&spec.Image, validation.Required),
		validation.Field(&spec.Ports, validation.By(validPorts)),
	)
	if err != nil {
		return fmt.Errorf("invalid container: %w", err)
	}

	return nil
}

func validateScript(spec api.ScriptSpec) error {
	source := spec.Source != nil && *spec.Source != ""
	raw := spec.Raw != nil && *spec.Raw != ""

	switch {
	case source && raw:
		return errors.New("invalid script: only one of source or raw may be specified")
	case !source && !raw:
		return errors.New("invalid script: one of source or raw is required")
	}

	if !source {
		return nil
	}

	if _, err := url.Parse(*spec.Source); err != nil {
		return fmt.Errorf("invalid script: source must be a valid url: %w", err)
	}

	return nil
}

// validPorts checks the port mappings with the same parser the docker driver uses,
// so a manifest that parses here cannot fail at the point the container is created.
func validPorts(value any) error {
	ports, ok := value.(*[]string)
	if !ok || ports == nil {
		return nil
	}

	if _, _, err := nat.ParsePortSpecs(*ports); err != nil {
		return fmt.Errorf("must be valid port mappings: %w", err)
	}

	return nil
}

// Runtime reports which runtime spec describes, which is determined by the block
// it carries rather than by a discriminator field.
//
// Returns ErrNoRuntime when no block is present, or ErrAmbiguousRuntime when more
// than one is.
func Runtime(spec api.WorkloadSpec) (api.Runtime, error) {
	switch {
	case spec.Container != nil && spec.Script != nil:
		return "", ErrAmbiguousRuntime
	case spec.Container != nil:
		return api.Container, nil
	case spec.Script != nil:
		return api.Script, nil
	default:
		return "", ErrNoRuntime
	}
}

func validCron(value any) error {
	schedule, ok := value.(*string)
	if !ok || schedule == nil || *schedule == "" {
		return nil
	}

	// The standard five-field form, matching what an operator would put in a
	// crontab, rather than the seconds-resolution variant.
	if _, err := cron.ParseStandard(*schedule); err != nil {
		return errors.New("must be a valid cron expression")
	}

	return nil
}
