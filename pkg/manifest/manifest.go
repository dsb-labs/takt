// Package manifest provides parsing and validation of takt resource manifests.
//
// A manifest is the YAML file an operator writes to describe a resource. Parsing
// it is a client-side concern: it produces the value the client submits, so the
// OpenAPI document remains the only description of the wire format and the server
// never has to understand YAML.
package manifest

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
	// maxLabelKeyLength is the cap every other name in takt is held to.
	maxLabelKeyLength = 63
	// maxLabelValueLength is counted in bytes, because the limit protects what
	// stores and displays the value rather than how many characters it reads
	// as. Generous enough for a URL or a one-line description.
	maxLabelValueLength = 256
	// reservedLabelPrefix marks the keys the docker driver writes for itself.
	reservedLabelPrefix = "takt."
)

// validateVersion reports whether a manifest's schema version is one this
// package understands. It stands apart from validateHeader for the one manifest
// kind that carries no name: the policy document names nothing, it is the whole
// policy.
func validateVersion(v string) error {
	if v == "" {
		return errors.New("invalid manifest: version is required")
	}

	if v != version {
		return fmt.Errorf("invalid manifest: version must be %q", version)
	}

	return nil
}

// validateHeader reports whether a manifest's version and name are usable, which
// every manifest kind requires the same way.
func validateHeader(v, name string) error {
	if err := validateVersion(v); err != nil {
		return err
	}

	if name == "" {
		return errors.New("invalid manifest: name is required")
	}

	if len(name) > 63 {
		return errors.New("invalid manifest: name must be at most 63 characters")
	}

	if !namePattern.MatchString(name) {
		return errors.New("invalid manifest: name must be lowercase alphanumeric, optionally separated by dashes")
	}

	return nil
}

// ValidateLabels reports whether the labels are ones takt will attach.
//
// Keys are held to the shape operators arrive with rather than to the workload
// name pattern, so app.kubernetes.io/name passes. Values are freer still — any
// printable text, since a label is read by people and compared as text by the
// list query filter — but control characters are refused because the value
// reaches container metadata and terminal output.
//
// The takt. prefix is refused for feedback rather than safety. The docker driver
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
			return fmt.Errorf("invalid labels: key %q uses the %q prefix, which is reserved for the labels takt writes itself",
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

// validProtocol reports whether a port names a protocol takt can publish it on.
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
