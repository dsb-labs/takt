package wire

import (
	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// ToVolume maps a wire volume specification onto the canonical shape.
//
// Volumes crossed the wire without this package while they were no more than a
// name and labels: each side mapped the fields inline. An owner and a mode gave
// the server and the client the same mapping to make, which is what this package
// exists to hold once.
func ToVolume(spec api.VolumeSpec) manifest.Volume {
	out := manifest.Volume{
		Version: spec.Version,
		Name:    spec.Name,
	}

	if spec.Labels != nil {
		out.Labels = *spec.Labels
	}
	if spec.Owner != nil {
		out.Owner = *spec.Owner
	}
	if spec.Mode != nil {
		out.Mode = *spec.Mode
	}

	return out
}

// FromVolume maps a canonical volume specification onto the wire shape, for a
// client submitting one.
//
// Empty optional values are sent as absent rather than as empty ones, matching
// what FromSpec does for a workload.
func FromVolume(volume manifest.Volume) api.VolumeSpec {
	out := api.VolumeSpec{
		Version: volume.Version,
		Name:    volume.Name,
	}

	if len(volume.Labels) > 0 {
		out.Labels = new(api.Labels(volume.Labels))
	}
	if volume.Owner != "" {
		out.Owner = new(volume.Owner)
	}
	if volume.Mode != "" {
		out.Mode = new(volume.Mode)
	}

	return out
}
