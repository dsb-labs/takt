package manifest

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type (
	// The Volume type describes a volume: a name, and what the directory backing
	// it looks like to the workloads writing into it.
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
		// Who owns the volume's directory, written as a numeric "uid" or
		// "uid:gid". This is what lets an image running as a fixed non-root
		// user write to the volume it mounts. Empty leaves the directory owned
		// by the user running the server.
		//
		// Numeric on purpose. A name would resolve against the host's user
		// database, so the same manifest would mean different users on
		// different hosts.
		Owner string `json:"owner,omitempty"`
		// The permission bits on the volume's directory, written as an octal
		// string such as "0755". A string rather than a number, so the digits
		// survive as written: a number field would carry a value whose base
		// depends on how the parser read it, and 755 read as decimal is not a
		// mode anyone meant. Empty leaves the directory readable only by the
		// user running the server.
		Mode string `json:"mode,omitempty"`
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

	if err := ValidateLabels(volume.Labels); err != nil {
		return err
	}

	if err := validateOwner(volume.Owner); err != nil {
		return err
	}

	return validateMode(volume.Mode)
}

// The shape an owner takes: a numeric user identifier, optionally with a numeric
// group identifier.
var ownerPattern = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)

// validateOwner reports whether a volume's owner names identifiers chown could
// apply.
//
// Each part is proved to fit an identifier here, so a number too large to be one
// is refused rather than truncated by whoever applies it.
func validateOwner(owner string) error {
	if owner == "" {
		return nil
	}

	if !ownerPattern.MatchString(owner) {
		return fmt.Errorf("invalid volume: owner %q must be a numeric uid or uid:gid", owner)
	}

	for part := range strings.SplitSeq(owner, ":") {
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return fmt.Errorf("invalid volume: owner %q is not an identifier the host can hold", owner)
		}
	}

	return nil
}

// validateMode reports whether a volume's mode is a directory mode chmod could
// apply.
//
// Up to four octal digits, so a shared volume can carry the setgid bit. The
// digits are proved to parse here so the service applying them never has to ask
// what a stored mode means.
func validateMode(mode string) error {
	if mode == "" {
		return nil
	}

	if _, err := strconv.ParseUint(mode, 8, 32); err != nil || len(mode) > 4 {
		return fmt.Errorf("invalid volume: mode %q must be an octal mode such as \"0755\"", mode)
	}

	return nil
}
