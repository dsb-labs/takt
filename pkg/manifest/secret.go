package manifest

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

var (
	// ErrInvalidReference is returned when a value holds something that begins like a
	// secret reference but is not one.
	ErrInvalidReference = errors.New("invalid secret reference")
	// ErrUnknownSecret is returned when expanding a value that references a secret
	// the caller could not resolve.
	ErrUnknownSecret = errors.New("unknown secret")
)

const (
	// What opens a reference, after the escape has been ruled out.
	referencePrefix = "${secret:"
	// What a reference is closed by.
	referenceSuffix = '}'
	// What introduces a reference, and what doubles to mean itself.
	referenceSigil = '$'
)

// ParseReferences returns the names of the secrets value references, in the order
// they appear and without repeats.
//
// The grammar is whole. A reference is "${secret:name}", and "$$" is a literal
// dollar sign. Anything else following an unescaped dollar sign is reported rather
// than passed through, so a manifest that meant to reference a secret is never
// quietly handed the text it wrote. This is deliberately not a template language:
// there is nothing to evaluate, so there is no way to ask it to.
func ParseReferences(value string) ([]string, error) {
	var names []string

	for i := 0; i < len(value); i++ {
		if value[i] != referenceSigil {
			continue
		}

		// An escaped sigil stands for itself and introduces nothing.
		if i+1 < len(value) && value[i+1] == referenceSigil {
			i++
			continue
		}

		rest := value[i:]
		if !strings.HasPrefix(rest, referencePrefix) {
			return nil, fmt.Errorf("%w: %q must be %q, or %q for a literal dollar sign",
				ErrInvalidReference, truncate(rest), referencePrefix+"name}", "$$")
		}

		end := strings.IndexByte(rest, referenceSuffix)
		if end < 0 {
			return nil, fmt.Errorf("%w: %q is not closed by %q",
				ErrInvalidReference, truncate(rest), string(referenceSuffix))
		}

		name := rest[len(referencePrefix):end]
		if !namePattern.MatchString(name) || len(name) > 63 {
			return nil, fmt.Errorf("%w: secret name %q must be lowercase alphanumeric, optionally separated by dashes",
				ErrInvalidReference, name)
		}

		if !slices.Contains(names, name) {
			names = append(names, name)
		}

		i += end
	}

	return names, nil
}

// Expand replaces every secret reference in value with what resolve returns for it,
// and unescapes each "$$" to a single dollar sign.
//
// Returns ErrUnknownSecret naming the secret when resolve reports it does not hold
// one. Leaving the reference text in place would hand a workload the reference as
// though it were the value, which it would then use.
func Expand(value string, resolve func(name string) (string, bool)) (string, error) {
	// Checked up front so that a malformed reference is reported the same way
	// wherever expansion happens, rather than only where a scan happened to look.
	if _, err := ParseReferences(value); err != nil {
		return "", err
	}

	var out strings.Builder
	out.Grow(len(value))

	for i := 0; i < len(value); i++ {
		if value[i] != referenceSigil {
			out.WriteByte(value[i])
			continue
		}

		if i+1 < len(value) && value[i+1] == referenceSigil {
			out.WriteByte(referenceSigil)
			i++

			continue
		}

		rest := value[i:]
		end := strings.IndexByte(rest, referenceSuffix)
		name := rest[len(referencePrefix):end]

		resolved, ok := resolve(name)
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrUnknownSecret, name)
		}

		out.WriteString(resolved)

		i += end
	}

	return out.String(), nil
}

// References returns the names of every secret the workload's environment
// references, sorted so that the result is stable.
//
// Stable because these names reach the hash of a workload's specification. An order
// that depended on map iteration would make an unchanged workload hash differently
// each time it was applied.
func References(spec Spec) ([]string, error) {
	var names []string

	// Sorted so that a manifest with two bad references always reports the same one.
	for _, key := range slices.Sorted(maps.Keys(spec.Env)) {
		found, err := ParseReferences(spec.Env[key])
		if err != nil {
			return nil, fmt.Errorf("invalid env %s: %w", key, err)
		}

		for _, name := range found {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}

	slices.Sort(names)

	return names, nil
}

// validateEnv reports whether the workload's environment holds usable secret
// references.
//
// Only the values are scanned. A key is the name of an environment variable rather
// than something a workload reads, so a reference in one has nothing to substitute
// into and is left as the literal text it is.
func validateEnv(spec Spec) error {
	_, err := References(spec)

	return err
}

// truncate shortens a value for an error message, so that a reference opened in a
// long string does not quote the whole thing back.
func truncate(value string) string {
	const limit = 32

	if len(value) <= limit {
		return value
	}

	return value[:limit] + "..."
}
