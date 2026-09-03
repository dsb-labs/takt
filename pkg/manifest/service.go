package manifest

import (
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

type (
	// The Service type describes a service, which names a set of workload
	// instances to balance requests across. The instances are selected by their
	// workload's labels rather than named, so a service can span every workload
	// that carries them.
	Service struct {
		// The manifest schema version. Must be "v1".
		Version string `json:"version"`
		// The name that identifies the service.
		Name string `json:"name"`
		// Arbitrary key-value pairs attached to the service. Held to the same
		// rules as a workload's, so an operator learns one answer.
		Labels map[string]string `json:"labels,omitempty"`
		// Which workload instances the service selects, and which of their
		// ports it addresses.
		Target ServiceTarget `json:"target"`
	}

	// The ServiceTarget type describes what a service selects.
	ServiceTarget struct {
		// The labels a workload must carry for its instances to be selected.
		// A workload matches when it carries every one of them, and at least
		// one is required: a service selecting everything is more likely a
		// mistake than an intent.
		Labels map[string]string `json:"labels"`
		// The port the selected instances listen on, written as the port
		// inside the workload. Always a number rather than a port's name,
		// because the selected workloads need not agree on their port names.
		Port int `json:"port"`
		// The transport protocol of the port. Defaults to TCP.
		Protocol Protocol `json:"protocol,omitempty"`
	}
)

// Reserved as a service name because the service API serves its own routes under
// the path a service of that name would occupy.
const reservedServiceName = "stream"

// ParseService reads a service manifest from r and returns the service it
// describes.
//
// A service manifest carries no kind field, as a volume manifest does not. Which
// resource a file describes is decided by what it is given to, so a file naming
// another resource's fields is reported as having unknown keys.
func ParseService(r io.Reader) (Service, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var service Service
	if err := decoder.Decode(&service); err != nil {
		return Service{}, fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Values left unset in the file are resolved before validation, so the rules
	// check what will actually be used rather than zeroes.
	service.Defaults()

	if err := ValidateService(service); err != nil {
		return Service{}, err
	}

	return service, nil
}

// ValidateService reports whether service is a usable service specification.
func ValidateService(service Service) error {
	if err := validateHeader(service.Version, service.Name); err != nil {
		return err
	}

	if service.Name == reservedServiceName {
		return fmt.Errorf("invalid manifest: the name %q is reserved", reservedServiceName)
	}

	if err := ValidateLabels(service.Labels); err != nil {
		return err
	}

	return validateTarget(service.Target)
}

// validateTarget reports whether the target selects something a service can
// address.
func validateTarget(target ServiceTarget) error {
	if len(target.Labels) == 0 {
		return errors.New("invalid target: at least one label is required")
	}

	if err := ValidateLabels(target.Labels); err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}

	if err := validPort(target.Port, "port"); err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}

	if err := validProtocol(target.Protocol); err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}

	return nil
}

// Defaults fills in what the service left unset, so that validation checks what
// will actually be used rather than zeroes.
//
// Exported because it runs however a Service was built, decoded from YAML by
// ParseService or converted from the wire format by the caller that received
// one. A default applied on only one of those paths would make the same
// manifest behave differently depending on how it reached the server.
func (s *Service) Defaults() {
	if s.Target.Protocol == "" {
		s.Target.Protocol = ProtocolTCP
	}
}
