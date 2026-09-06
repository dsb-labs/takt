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
	// ErrUnknownWorkload is returned when expanding a value that references a
	// workload the caller could not resolve.
	ErrUnknownWorkload = errors.New("unknown workload")
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
		// The name of the secret, variable or workload being referenced.
		Name string
		// Which of the referenced workload's ports is wanted, which resolves the
		// reference to an address rather than to a host. Empty for a reference of
		// any other kind, which has no port to name.
		Port PortRef
	}
)

const (
	// KindSecret is a reference to a value takt holds encrypted, which nothing reads
	// back out.
	KindSecret ReferenceKind = "secret"
	// KindVariable is a reference to a value takt holds in the clear, which the API
	// reports.
	KindVariable ReferenceKind = "var"
	// KindWorkload is a reference to the address another workload is reached at.
	//
	// Unlike the other two it resolves against something takt settled on rather than
	// something an operator stored, which is what lets a workload be written down as
	// the dependency of another without either naming a port takt chose.
	KindWorkload ReferenceKind = "workload"
)

// Every kind a reference may name.
//
// A slice rather than a set so that the error naming the accepted forms lists them
// the same way each time. No kind's opening is a prefix of another's, so the order
// does not affect what matches.
var referenceKinds = []ReferenceKind{KindSecret, KindVariable, KindWorkload}

// opening returns the text that opens a reference of this kind.
//
// Derived from the kind rather than held beside it, so the two cannot disagree.
func (k ReferenceKind) opening() string {
	return "${" + string(k) + ":"
}

// ParseReferences returns the references value holds, in the order they appear and
// without repeats.
//
// The grammar is whole. A reference is "${secret:name}", "${var:name}",
// "${workload:name}" or "${workload:name:port}", and "$$" is a literal dollar sign.
// Anything else following an unescaped dollar sign is reported rather than passed
// through, so a manifest that meant to reference something is never quietly handed
// the text it wrote. This is deliberately not a template language: there is nothing
// to evaluate, so there is no way to ask it to.
//
// Only a workload reference may name a port, since it is the only kind that resolves
// against something publishing one.
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

		reference, err := newReference(kind, rest[len(prefix):end])
		if err != nil {
			return nil, err
		}

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
// Returns ErrUnknownSecret, ErrUnknownVariable or ErrUnknownWorkload naming the
// reference when resolve reports it holds nothing for one. Leaving the reference text
// in place would hand a workload the reference as though it were the value, which it
// would then use.
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

		// ParseReferences has already rejected anything malformed, so the reference
		// is well-formed however this value was reached.
		reference, _ := newReference(kind, rest[len(prefix):end])

		resolved, ok := resolve(reference)
		if !ok {
			return "", fmt.Errorf("%w: %s", unknown(kind), reference)
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
// holding it. Whether a change replaces the instance or refreshes the file is a
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
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Port, b.Port))
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
// replaced, so a reference with any such reading has to move the hash. Refreshing the
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

// Names returns the names of the references of the given kind, without repeats and
// keeping the order they were given in.
//
// Filtered here rather than by each caller so that a set of names is collected the
// same way wherever one kind has to be told from the other.
//
// Repeats are dropped because one workload may be referenced more than once, at a
// different port each time. Those are different references reaching one name, and a
// caller asking what a specification reads wants the name once.
func Names(references []Reference, kind ReferenceKind) []string {
	var names []string
	for _, reference := range references {
		if reference.Kind == kind && !slices.Contains(names, reference.Name) {
			names = append(names, reference.Name)
		}
	}

	return names
}

// Of returns the references of the given kind, keeping the order they were given in.
//
// This exists for a caller that needs the port a workload reference names as well as
// the workload it names, which Names cannot report.
func Of(references []Reference, kind ReferenceKind) []Reference {
	var found []Reference
	for _, reference := range references {
		if reference.Kind == kind {
			found = append(found, reference)
		}
	}

	return found
}

// String returns the reference as it is written inside its opening and closing
// braces, which is how one is named by an error and keyed by whatever records what a
// workload reads.
func (r Reference) String() string {
	if r.Port == "" {
		return r.Name
	}

	return r.Name + ":" + string(r.Port)
}

// newReference builds a reference of the given kind from the text between its opening
// and the brace that closes it.
func newReference(kind ReferenceKind, body string) (Reference, error) {
	name, port, qualified := strings.Cut(body, ":")

	switch {
	case qualified && kind != KindWorkload:
		return Reference{}, fmt.Errorf("%w: %q names a port, which only a %s reference may do",
			ErrInvalidReference, body, KindWorkload)
	case !validReferenceName(name):
		return Reference{}, fmt.Errorf("%w: %s name %q must be lowercase alphanumeric, optionally separated by dashes",
			ErrInvalidReference, kind, name)
	case qualified && !validReferenceName(port):
		return Reference{}, fmt.Errorf("%w: port %q of %s %q must be a port name or a port number",
			ErrInvalidReference, port, kind, name)
	}

	return Reference{Kind: kind, Name: name, Port: PortRef(port)}, nil
}

// validReferenceName reports whether a reference names something takt could hold
// under that name.
func validReferenceName(name string) bool {
	return namePattern.MatchString(name) && len(name) <= maxLabelKeyLength
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
	switch kind {
	case KindVariable:
		return ErrUnknownVariable
	case KindWorkload:
		return ErrUnknownWorkload
	default:
		return ErrUnknownSecret
	}
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
