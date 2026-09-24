// Package score provides parsing and rendering of a score: a file naming the
// manifests, variables and secrets that make up one deployment, with Go
// templating over a values file.
//
// A score is a client-side build step. Rendering it produces ordinary manifests,
// and applying it is ordinary API calls in dependency order. The server never
// learns what a score is: what ties the resources together is a pair of labels
// on each one, which is what delete, prune and list work from.
package score

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Score type describes a score file: the manifests, variables and secrets
	// that belong to one deployment.
	Score struct {
		// The score schema version. Must be "v1".
		Version string `yaml:"version"`
		// The name of the package the score describes. The install name defaults
		// to it, and templates read it as .Score.Package.
		Name string `yaml:"name"`
		// The version of the package, which templates read as .Score.Release and
		// every applied resource is labelled with.
		Release string `yaml:"release"`
		// The volume manifests, as paths relative to the score.
		Volumes []string `yaml:"volumes"`
		// The workload manifests, as paths relative to the score.
		Workloads []string `yaml:"workloads"`
		// The service manifests, as paths relative to the score.
		Services []string `yaml:"services"`
		// The variables the score owns or requires.
		Variables []Variable `yaml:"variables"`
		// The secrets the score's workloads read. Always declare-only: a value
		// never enters a score.
		Secrets []Secret `yaml:"secrets"`
		// The directory the score was loaded from, which every path in it is
		// relative to. Set by Load rather than read from the file.
		Directory string `yaml:"-"`
	}

	// The Variable type is one entry in a score's variables list.
	//
	// An entry with a file or a value is owned by the score: the applier sets it,
	// and it is overwritten on every apply. An entry with neither is required, and
	// must already exist when the score is applied.
	Variable struct {
		// The name of the variable.
		Name string `yaml:"name"`
		// A file whose rendered contents become the variable's value, as a path
		// relative to the score.
		File string `yaml:"file,omitempty"`
		// The variable's value, rendered as a template.
		Value string `yaml:"value,omitempty"`
	}

	// The Secret type is one entry in a score's secrets list. It names a secret the
	// applier must have set, and nothing more.
	Secret struct {
		// The name of the secret.
		Name string `yaml:"name"`
	}
)

const (
	// The score schema version this package understands.
	version = "v1"
	// The file a score is read from when a directory is given.
	scoreFile = "score.yaml"
	// The file holding a score's default values, found beside the score by
	// convention rather than named in it.
	valuesFile = "values.yaml"
)

const (
	// LabelName is the label every applied resource carries, holding the name
	// the score was installed as.
	LabelName = "score"
	// LabelRelease is the label every applied resource carries, holding the
	// release of the score that last applied it.
	LabelRelease = "score.release"
)

// Load reads the score at the given location, which is either a directory
// holding a score.yaml or the path of the score file itself.
func Load(location string) (Score, error) {
	path := location

	info, err := os.Stat(location)
	if err != nil {
		return Score{}, fmt.Errorf("failed to find score: %w", err)
	}

	if info.IsDir() {
		path = filepath.Join(location, scoreFile)
	}

	f, err := os.Open(path)
	if err != nil {
		return Score{}, fmt.Errorf("failed to open score: %w", err)
	}
	defer f.Close()

	score, err := Parse(f)
	if err != nil {
		return Score{}, err
	}

	score.Directory = filepath.Dir(path)

	return score, nil
}

// Parse reads a score from r and returns the score it describes.
//
// The result records no directory, so the caller resolves the paths in it.
// Load is the usual way in and does that for you.
func Parse(r io.Reader) (Score, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var score Score
	if err := decoder.Decode(&score); err != nil {
		return Score{}, fmt.Errorf("failed to parse score: %w", err)
	}

	if err := Validate(score); err != nil {
		return Score{}, err
	}

	return score, nil
}

// Validate reports whether score is a usable score.
//
// Only the file's own shape is checked here. Whether the manifests it names exist
// and parse is answered by Render, which is where they are read.
func Validate(score Score) error {
	if score.Version == "" {
		return errors.New("invalid score: version is required")
	}

	if score.Version != version {
		return fmt.Errorf("invalid score: version must be %q", version)
	}

	if err := manifest.ValidateName(score.Name); err != nil {
		return fmt.Errorf("invalid score: %w", err)
	}

	if score.Release == "" {
		return errors.New("invalid score: release is required")
	}

	if err := manifest.ValidateLabels(map[string]string{LabelRelease: score.Release}); err != nil {
		return fmt.Errorf("invalid score: release %q is not usable as a label: %w", score.Release, err)
	}

	if err := validatePaths(score); err != nil {
		return err
	}

	if err := validateVariables(score.Variables); err != nil {
		return err
	}

	return validateSecrets(score.Secrets)
}

// validatePaths reports whether every manifest path stays inside the score.
//
// A path that climbs out of the directory is refused rather than followed. A score
// is meant to be a self-contained unit that can be handed around, and one that
// reaches for a file beside it is not.
func validatePaths(score Score) error {
	for _, path := range score.Volumes {
		if err := validatePath(path); err != nil {
			return fmt.Errorf("invalid score: volume %w", err)
		}
	}

	for _, path := range score.Workloads {
		if err := validatePath(path); err != nil {
			return fmt.Errorf("invalid score: workload %w", err)
		}
	}

	for _, path := range score.Services {
		if err := validatePath(path); err != nil {
			return fmt.Errorf("invalid score: service %w", err)
		}
	}

	for _, variable := range score.Variables {
		if variable.File == "" {
			continue
		}

		if err := validatePath(variable.File); err != nil {
			return fmt.Errorf("invalid score: variable %s %w", variable.Name, err)
		}
	}

	return nil
}

func validatePath(path string) error {
	switch {
	case path == "":
		return errors.New("path is required")
	case filepath.IsAbs(path):
		return fmt.Errorf("path %q must be relative to the score", path)
	case filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), "../"):
		return fmt.Errorf("path %q must stay inside the score's directory", path)
	default:
		return nil
	}
}

// validateVariables reports whether every variable entry names one thing and no
// name is listed twice.
func validateVariables(variables []Variable) error {
	seen := make(map[string]struct{}, len(variables))
	for _, variable := range variables {
		if err := manifest.ValidateName(variable.Name); err != nil {
			return fmt.Errorf("invalid score: variable %w", err)
		}

		if variable.File != "" && variable.Value != "" {
			return fmt.Errorf("invalid score: variable %s names both a file and a value", variable.Name)
		}

		if _, ok := seen[variable.Name]; ok {
			return fmt.Errorf("invalid score: variable %s is listed twice", variable.Name)
		}

		seen[variable.Name] = struct{}{}
	}

	return nil
}

// validateSecrets reports whether every secret entry has a usable name and no
// name is listed twice.
func validateSecrets(secrets []Secret) error {
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if err := manifest.ValidateName(secret.Name); err != nil {
			return fmt.Errorf("invalid score: secret %w", err)
		}

		if _, ok := seen[secret.Name]; ok {
			return fmt.Errorf("invalid score: secret %s is listed twice", secret.Name)
		}

		seen[secret.Name] = struct{}{}
	}

	return nil
}

// Owned reports whether the variable is one the score sets, as opposed to one it
// requires the applier to have set.
func (v Variable) Owned() bool {
	return v.File != "" || v.Value != ""
}
