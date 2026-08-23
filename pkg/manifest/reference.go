package manifest

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

var (
	// ErrInvalidReference is returned when a value holds something that begins like a
	// reference but is not one.
	ErrInvalidReference = errors.New("invalid reference")
	// ErrUnknownSecret is returned when expanding a value that references a secret
	// the caller could not resolve.
	ErrUnknownSecret = errors.New("unknown secret")
	// ErrUnknownVariable is returned when expanding a value that references a
	// variable the caller could not resolve.
	ErrUnknownVariable = errors.New("unknown variable")
)

const (
	// What a reference is closed by.
	referenceSuffix = '}'
	// What introduces a reference, and what doubles to mean itself.
	referenceSigil = '$'
)

type (
	// The ReferenceKind type names what a reference resolves against.
	ReferenceKind string

	// The Reference type identifies one thing a value in a manifest reads rather
	// than holds.
	//
	// The kind is part of the identity rather than a detail of the syntax. A secret
	// and a variable may share a name, and the two are resolved from different
	// places, so one is never a substitute for the other.
	Reference struct {
		// What the reference resolves against.
		Kind ReferenceKind
		// The name of the secret or variable being referenced.
		Name string
	}
)

const (
	// KindSecret is a reference to a value orca holds encrypted, which nothing reads
	// back out.
	KindSecret ReferenceKind = "secret"
	// KindVariable is a reference to a value orca holds in the clear, which the API
	// reports.
	KindVariable ReferenceKind = "var"
)

// Every kind a reference may name.
//
// A slice rather than a set so that the error naming the accepted forms lists them
// the same way each time. No kind's opening is a prefix of another's, so the order
// does not affect what matches.
var referenceKinds = []ReferenceKind{KindSecret, KindVariable}

// opening returns the text that opens a reference of this kind.
//
// Derived from the kind rather than held beside it, so the two cannot disagree.
func (k ReferenceKind) opening() string {
	return "${" + string(k) + ":"
}

// ParseReferences returns the references value holds, in the order they appear and
// without repeats.
//
// The grammar is whole. A reference is "${secret:name}" or "${var:name}", and "$$"
// is a literal dollar sign. Anything else following an unescaped dollar sign is
// reported rather than passed through, so a manifest that meant to reference
// something is never quietly handed the text it wrote. This is deliberately not a
// template language: there is nothing to evaluate, so there is no way to ask it to.
func ParseReferences(value string) ([]Reference, error) {
	var references []Reference

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

		prefix, kind, ok := openingOf(rest)
		if !ok {
			return nil, fmt.Errorf("%w: %q must be %s, or %q for a literal dollar sign",
				ErrInvalidReference, truncate(rest), acceptedForms(), "$$")
		}

		end := strings.IndexByte(rest, referenceSuffix)
		if end < 0 {
			return nil, fmt.Errorf("%w: %q is not closed by %q",
				ErrInvalidReference, truncate(rest), string(referenceSuffix))
		}

		name := rest[len(prefix):end]
		if !namePattern.MatchString(name) || len(name) > 63 {
			return nil, fmt.Errorf("%w: %s name %q must be lowercase alphanumeric, optionally separated by dashes",
				ErrInvalidReference, kind, name)
		}

		reference := Reference{Kind: kind, Name: name}
		if !slices.Contains(references, reference) {
			references = append(references, reference)
		}

		i += end
	}

	return references, nil
}

// Expand replaces every reference in value with what resolve returns for it, and
// unescapes each "$$" to a single dollar sign.
//
// Returns ErrUnknownSecret or ErrUnknownVariable naming the reference when resolve
// reports it holds nothing for one. Leaving the reference text in place would hand a
// workload the reference as though it were the value, which it would then use.
func Expand(value string, resolve func(reference Reference) (string, bool)) (string, error) {
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
		prefix, kind, _ := openingOf(rest)
		end := strings.IndexByte(rest, referenceSuffix)

		reference := Reference{Kind: kind, Name: rest[len(prefix):end]}

		resolved, ok := resolve(reference)
		if !ok {
			return "", fmt.Errorf("%w: %s", unknown(kind), reference.Name)
		}

		out.WriteString(resolved)

		i += end
	}

	return out.String(), nil
}

// References returns every reference the workload reads, from its environment and
// from what it mounts, sorted so that the result is stable.
//
// Stable because these references reach the hash of a workload's specification. An
// order that depended on map iteration would make an unchanged workload hash
// differently each time it was applied.
//
// A mounted secret or variable is included whatever its delivery mode. This is what
// records that the workload reads it, so deleting one still reports the workloads
// holding it; whether a change replaces the instance or refreshes the file is a
// separate question, which Refreshed answers.
func References(spec Spec) ([]Reference, error) {
	var references []Reference

	// Sorted so that a manifest with two bad references always reports the same one.
	for _, key := range slices.Sorted(maps.Keys(spec.Env)) {
		found, err := ParseReferences(spec.Env[key])
		if err != nil {
			return nil, fmt.Errorf("invalid env %s: %w", key, err)
		}

		for _, reference := range found {
			if !slices.Contains(references, reference) {
				references = append(references, reference)
			}
		}
	}

	for _, mount := range spec.Volumes {
		reference, ok := mount.Reference()
		if !ok {
			continue
		}

		if !slices.Contains(references, reference) {
			references = append(references, reference)
		}
	}

	slices.SortFunc(references, func(a, b Reference) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})

	return references, nil
}

// Refreshed returns the references the workload reads only through a mount that names
// a signal, sorted as References is.
//
// These are the references whose value must not reach the specification's hash. A
// change to one is delivered by rewriting the file and signalling the workload, and a
// hash that moved with it would have the reconciler replace the instance instead —
// which is the thing naming a signal asks not to happen.
//
// A reference read anywhere else as well is absent from the result. An environment
// variable is fixed once a process has started and another mount may ask to be
// replaced, so a reference with any such reading has to move the hash; refreshing the
// file it also appears at costs nothing and happens anyway.
func Refreshed(spec Spec) ([]Reference, error) {
	// Nothing can be refreshed without a mount asking for it, so a workload with no
	// mounts at all is answered without scanning its environment.
	signalled := make(map[Reference]struct{}, len(spec.Volumes))
	for _, mount := range spec.Volumes {
		if mount.Signal == "" {
			continue
		}

		if reference, ok := mount.Reference(); ok {
			signalled[reference] = struct{}{}
		}
	}

	if len(signalled) == 0 {
		return nil, nil
	}

	read, err := References(spec)
	if err != nil {
		return nil, err
	}

	// What is read in a way that a change cannot be delivered to in place. Built from
	// the same scan References made, rather than by parsing every env value again.
	replaced := make(map[Reference]struct{}, len(read))
	for key, value := range spec.Env {
		// References has already rejected a malformed value, so a failure here is
		// impossible rather than merely unlikely. Reported anyway, since silently
		// treating a value as holding no reference would put its value in the hash.
		found, err := ParseReferences(value)
		if err != nil {
			return nil, fmt.Errorf("invalid env %s: %w", key, err)
		}

		for _, reference := range found {
			replaced[reference] = struct{}{}
		}
	}

	for _, mount := range spec.Volumes {
		if mount.Signal != "" {
			continue
		}

		if reference, ok := mount.Reference(); ok {
			replaced[reference] = struct{}{}
		}
	}

	var references []Reference
	for _, reference := range read {
		if _, ok := signalled[reference]; !ok {
			continue
		}

		if _, ok := replaced[reference]; ok {
			continue
		}

		references = append(references, reference)
	}

	return references, nil
}

// Names returns the names of the references of the given kind, keeping the order
// they were given in.
//
// Filtered here rather than by each caller so that a set of names is collected the
// same way wherever one kind has to be told from the other.
func Names(references []Reference, kind ReferenceKind) []string {
	var names []string
	for _, reference := range references {
		if reference.Kind == kind {
			names = append(names, reference.Name)
		}
	}

	return names
}

// validateEnv reports whether the workload's environment holds usable references.
//
// Only the values are scanned. A key is the name of an environment variable rather
// than something a workload reads, so a reference in one has nothing to substitute
// into and is left as the literal text it is.
//
// A mount names what it reads directly rather than as reference text, so there is no
// syntax to reject there. validateVolumes checks those names.
func validateEnv(spec Spec) error {
	_, err := References(spec)

	return err
}

// openingOf reports which kind of reference rest opens, along with the text that
// opened it.
func openingOf(rest string) (string, ReferenceKind, bool) {
	for _, kind := range referenceKinds {
		if opening := kind.opening(); strings.HasPrefix(rest, opening) {
			return opening, kind, true
		}
	}

	return "", "", false
}

// acceptedForms names every reference an operator may write, for an error reporting
// something that is not one.
func acceptedForms() string {
	forms := make([]string, 0, len(referenceKinds))
	for _, kind := range referenceKinds {
		forms = append(forms, `"`+kind.opening()+`name}"`)
	}

	return strings.Join(forms, " or ")
}

// unknown returns the error reporting that a reference of the given kind resolved
// against nothing.
func unknown(kind ReferenceKind) error {
	if kind == KindVariable {
		return ErrUnknownVariable
	}

	return ErrUnknownSecret
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
