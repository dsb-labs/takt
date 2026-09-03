package manifest

import (
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

type (
	// The Volume type describes a volume, which is no more than its name. A volume
	// holds data and has nothing to configure.
	Volume struct {
		// The manifest schema version. Must be "v1".
		Version string `json:"version"`
		// The name that identifies the volume.
		Name string `json:"name"`
		// Arbitrary key-value pairs attached to the volume.
		//
		// Held to the same rules as a workload's, because an operator sorting
		// storage by which service owns it is doing the same thing they do with
		// workloads and should not have to learn a second answer.
		Labels map[string]string `json:"labels,omitempty"`
	}
)

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
	if err := validateHeader(volume.Version, volume.Name); err != nil {
		return err
	}

	return ValidateLabels(volume.Labels)
}
