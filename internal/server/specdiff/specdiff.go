// Package specdiff provides the comparison of two workload specifications that
// tells an operator what an apply would change.
//
// It works on the canonical encoding a specification hash is computed over, which is
// what the database stores. Comparing the encodings rather than the manifests means
// what is reported as changed is what the hash was computed from, so a specification
// reported as identical is one the hash agrees is identical.
//
// It is a package rather than a function beside its caller for the same reason
// spechash is: what it returns is read by an operator deciding whether to apply, and
// the tests here pin the paths it produces.
package specdiff

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// Changed names the fields that differ between two encoded specifications, as paths
// into the second.
//
// Both arguments are the canonical encoding a specification hash is computed over,
// so this compares what was stored against what an apply would store. Anything the
// encoding leaves out, such as a field carrying its zero value, is absent from both
// sides and so reads as unchanged.
//
// The paths are written in the syntax a list query uses, which is the syntax the
// unknown paths use as well. An operator meets one path syntax rather than two.
//
// The paths named in skip are dropped from the result. They are the values takt
// settles only as it applies: such a value is missing from the specification being
// reported and present in the one stored, which is not a change to anything the
// operator wrote.
//
// A field the two documents disagree about is named once, at the level the
// disagreement starts. A container block that was added is reported as the block
// rather than as each field inside it.
func Changed(stored, applying []byte, skip []string) ([]string, error) {
	var was, is any

	if err := json.Unmarshal(stored, &was); err != nil {
		return nil, fmt.Errorf("failed to decode the stored specification: %w", err)
	}

	if err := json.Unmarshal(applying, &is); err != nil {
		return nil, fmt.Errorf("failed to decode the reported specification: %w", err)
	}

	paths := slices.DeleteFunc(difference("$", was, is, nil), func(path string) bool {
		return slices.Contains(skip, path)
	})

	// Nil rather than the empty slice the deletion leaves behind, so that two
	// identical specifications and two whose only difference was skipped report the
	// same nothing.
	if len(paths) == 0 {
		return nil, nil
	}

	return paths, nil
}

// difference walks two decoded documents together and appends the path of every
// field they disagree about.
//
// Only the shapes json.Unmarshal produces are handled, because that is all either
// document holds. Two values of different shapes disagree by definition, so a field
// that became an object is reported without either side being walked.
func difference(path string, was, is any, paths []string) []string {
	switch was := was.(type) {
	case map[string]any:
		is, ok := is.(map[string]any)
		if !ok {
			return append(paths, path)
		}

		return objects(path, was, is, paths)
	case []any:
		is, ok := is.([]any)
		if !ok {
			return append(paths, path)
		}

		return arrays(path, was, is, paths)
	default:
		if was != is {
			return append(paths, path)
		}

		return paths
	}
}

// objects compares two decoded objects, key by key, over the keys either one holds.
//
// The keys are walked in order so that the result of a comparison is the same
// however Go happened to iterate the maps.
func objects(path string, was, is map[string]any, paths []string) []string {
	keys := make([]string, 0, len(was)+len(is))
	for key := range was {
		keys = append(keys, key)
	}

	for key := range is {
		if _, held := was[key]; !held {
			keys = append(keys, key)
		}
	}

	slices.Sort(keys)

	for _, key := range keys {
		before, held := was[key]
		after, holds := is[key]

		// A key on one side alone is a field that was added or removed, which is
		// reported as the field rather than walked into.
		if !held || !holds {
			paths = append(paths, path+"."+key)
			continue
		}

		paths = difference(path+"."+key, before, after, paths)
	}

	return paths
}

// arrays compares two decoded arrays by position.
//
// Position rather than content, because an element's index is how the rest of takt
// addresses it: the path of a port is the path a list query would match it by. An
// element inserted at the front therefore reports every position after it as
// changed, which is what an operator reading the paths against the reported
// specification will find there.
func arrays(path string, was, is []any, paths []string) []string {
	for i := range max(len(was), len(is)) {
		at := path + "[" + strconv.Itoa(i) + "]"

		switch {
		case i >= len(was) || i >= len(is):
			paths = append(paths, at)
		default:
			paths = difference(at, was[i], is[i], paths)
		}
	}

	return paths
}
