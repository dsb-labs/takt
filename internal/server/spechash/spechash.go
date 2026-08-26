// Package spechash provides the hash orca computes over a workload's specification.
//
// The hash decides whether a running instance is destroyed and replaced, which gives
// everything here a constraint almost nothing else in the codebase has: what it
// produces today it must produce forever. A field reordered, a zero value newly
// included, an omitempty dropped, and every running instance on every node is
// replaced the next time the reconciler passes.
//
// That is why this is a package rather than a pair of functions beside the code that
// calls them. The golden tests turn an accidental change into a failing test, and
// anything added here has to earn its place against a fixture that already pins the
// answer.
package spechash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/dsb-labs/orca/pkg/manifest"
)

type (
	// The Inputs type contains what reaches a specification's hash without being
	// stored alongside it.
	//
	// Each field is something orca resolved rather than something the operator
	// wrote. The specification is stored on its own, and this travels with it only
	// as far as the hash.
	Inputs struct {
		// The revision of each secret the workload reads, keyed by name.
		Revisions map[string]string
		// The value of each variable the workload reads, keyed by name.
		Values map[string]string
		// The address each workload reference resolved to, keyed by the reference as
		// it was written.
		Addresses map[string]string
		// What the workload reads only through a mount naming a signal, which is
		// removed from the revisions and the values before they are hashed.
		Refreshed []manifest.Reference
		// The digest the image's registry reports, set only for a workload whose
		// pull policy is always.
		Digest string
	}

	// The hashedSpec type is what a workload's specification hash is computed over
	// when the workload reads a secret or a variable.
	//
	// It exists so that what a workload reads reaches the hash without being stored:
	// the specification is written to the database on its own, and this wrapper is
	// built only to be hashed and discarded. Its shape is therefore part of what a
	// hash means — changing it re-hashes every workload that reads either and
	// replaces their instances.
	//
	// Both maps are omitted when empty, which is what stopped adding variables from
	// re-hashing every workload that already read a secret. Such a workload encodes
	// exactly as it did before variables existed, because the field that would have
	// been written as null is left out instead.
	hashedSpec struct {
		// The stored specification, encoded exactly as it is persisted.
		Spec json.RawMessage `json:"spec"`
		// The revision of each secret the workload reads, keyed by name. Go's encoder
		// sorts map keys, so this contributes the same bytes for a given set of
		// secrets however they were collected.
		Secrets map[string]string `json:"secrets,omitempty"`
		// The value of each variable the workload reads, keyed by name.
		//
		// The value rather than a revision, unlike a secret. A secret is held at arm's
		// length because the hash is reported and one computed over a value would
		// confirm a guess at it; a variable's value is reported by the API anyway, so
		// the indirection would protect nothing and cost a column.
		Variables map[string]string `json:"variables,omitempty"`
		// The address each workload reference resolved to, keyed by the reference as
		// it was written. Omitted when the workload references none.
		//
		// This is what makes a reallocated host port replace the instances reading
		// it: the address they were started with is part of what they are, so one
		// that moved is a specification that changed.
		Workloads map[string]string `json:"workloads,omitempty"`
		// The digest the image's registry reports, set only for a workload whose pull
		// policy is always. It reaches the hash without being stored, the way a
		// secret's revision does, so a rebuilt tag replaces the instance without the
		// digest being echoed back as though the operator wrote it.
		Digest string `json:"digest,omitempty"`
	}
)

// Compute encodes spec as JSON and hashes it, mixing in the revision of each secret
// and the value of each variable the workload reads. Go's encoder writes struct
// fields in declaration order and map keys in sorted order, so the encoding is stable
// for a given specification and the hash can be compared to detect drift.
//
// The returned bytes are always the specification alone: what was read reaches the
// hash without being stored, so nothing about a secret is written to the database or
// echoed back by the API. Mixing it in is what makes a rotated secret or a changed
// variable read as an ordinary specification change, so the reconciler replaces the
// instances holding the old value.
//
// A secret contributes its revision and never its value, because the hash is
// reported and one computed over a value would confirm a guess at it. A variable
// contributes its value, which the API reports anyway.
//
// What the workload reads only through a mount naming a signal contributes nothing.
// Such a mount asked for the file to be rewritten and the workload signalled, and a
// hash that moved with the value would replace the instance instead.
//
// The address each referenced workload resolved to is mixed in the same way. That is
// what makes a reallocated host port replace the instances reading it: the address
// they were started with is part of what they are.
//
// A pull-always workload's image digest is mixed in the same way, so a rebuilt tag
// reads as an ordinary specification change. It is empty for every other workload.
//
// A workload reading none of these hashes exactly as it would without this, which is
// what stops an upgrade replacing every running instance.
func Compute(spec manifest.Spec, inputs Inputs) ([]byte, string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode workload spec: %w", err)
	}

	revisions, values := inputs.hashed()

	if len(revisions) == 0 && len(values) == 0 && len(inputs.Addresses) == 0 && inputs.Digest == "" {
		sum := sha256.Sum256(encoded)

		return encoded, hex.EncodeToString(sum[:]), nil
	}

	hashed, err := json.Marshal(hashedSpec{
		Spec:      encoded,
		Secrets:   revisions,
		Variables: values,
		Workloads: inputs.Addresses,
		Digest:    inputs.Digest,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode workload spec: %w", err)
	}

	sum := sha256.Sum256(hashed)

	return encoded, hex.EncodeToString(sum[:]), nil
}

// hashed returns what reaches the specification's hash: the revision of each secret
// and the value of each variable, less anything read only through a mount naming a
// signal.
//
// Both maps come back nil when nothing is left, rather than empty. hashedSpec omits an
// empty map, so a workload whose only reading is refreshed hashes exactly as one that
// reads nothing at all — which is what keeps adding a signalling mount from being a
// specification change in its own right.
func (i Inputs) hashed() (map[string]string, map[string]string) {
	if len(i.Refreshed) == 0 {
		return i.Revisions, i.Values
	}

	revisions := maps.Clone(i.Revisions)
	values := maps.Clone(i.Values)

	for _, reference := range i.Refreshed {
		if reference.Kind == manifest.KindVariable {
			delete(values, reference.Name)

			continue
		}

		delete(revisions, reference.Name)
	}

	if len(revisions) == 0 {
		revisions = nil
	}
	if len(values) == 0 {
		values = nil
	}

	return revisions, values
}
