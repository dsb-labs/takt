package score

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// The Values type is what templates read as .Values: the merged contents of the
// score's default values file and every values file given on top of it.
type Values map[string]any

// Values returns the score's values: the values.yaml beside the score, when there
// is one, with each of the given files merged over it in order.
//
// Mappings merge key by key, so a file can override one setting without
// restating its siblings. Anything else — a sequence, a scalar — is replaced
// whole. The paths are taken as given rather than resolved against the score,
// since they are what an operator typed.
func (s Score) Values(files ...string) (Values, error) {
	values := make(Values)

	if s.Directory != "" {
		defaults, err := readValues(filepath.Join(s.Directory, valuesFile))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		default:
			values.Merge(defaults)
		}
	}

	for _, file := range files {
		overrides, err := readValues(file)
		if err != nil {
			return nil, err
		}

		values.Merge(overrides)
	}

	return values, nil
}

// Merge folds other into v, merging mappings key by key and replacing anything
// else. Values already in v that other does not name are kept.
func (v Values) Merge(other Values) {
	for key, value := range other {
		theirs, ok := value.(map[string]any)
		if !ok {
			v[key] = value
			continue
		}

		ours, ok := v[key].(map[string]any)
		if !ok {
			v[key] = maps.Clone(theirs)
			continue
		}

		merged := Values(ours)
		merged.Merge(theirs)
		v[key] = map[string]any(merged)
	}
}

// readValues reads one values file. An empty file is an empty set of values
// rather than an error, since a values file that has nothing to say yet is
// still a values file.
func readValues(path string) (Values, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read values: %w", err)
	}

	// Decoded into the plain map type rather than into Values, because the
	// decoder builds every nested mapping as the type it was given, and Merge
	// tells a mapping apart from a scalar by the plain type.
	var values map[string]any
	if err = yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("failed to parse values %s: %w", path, err)
	}

	return values, nil
}
