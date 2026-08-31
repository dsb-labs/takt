package spechash_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/golden"

	"github.com/dsb-labs/orca/internal/server/spechash"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// TestCompute pins a known specification to a known hash.
//
// The hash decides whether a running instance is destroyed and replaced, so a failure
// here means every workload on every node is about to be replaced when operators
// upgrade. The golden files are regenerated with -update like any others, but do that
// only once the change that moved them is one you meant to make. Refreshing them to
// make the suite green is how a fleet-wide redeploy ships unnoticed.
func TestCompute(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name   string
		File   string
		Inputs spechash.Inputs
	}{
		{
			// The case every other one is measured against. A workload reading
			// nothing hashes over its specification alone, with no wrapper, which is
			// what stops an upgrade replacing every running instance.
			Name: "reads nothing",
			File: "plain.json",
		},
		{
			Name:   "reads only secrets",
			File:   "reads_secret.json",
			Inputs: spechash.Inputs{Revisions: map[string]string{"db-password": "rev-one"}},
		},
		{
			Name:   "reads only variables",
			File:   "reads_variable.json",
			Inputs: spechash.Inputs{Values: map[string]string{"region": "eu-west-1"}},
		},
		{
			Name: "reads both",
			File: "reads_both.json",
			Inputs: spechash.Inputs{
				Revisions: map[string]string{"db-password": "rev-one"},
				Values:    map[string]string{"region": "eu-west-1"},
			},
		},
		{
			// The boundary the omitempty on both maps turns on. What is read only
			// through a mount naming a signal is removed before hashing, so this
			// hashes as a workload reading nothing at all.
			Name: "reads only through a signalling mount",
			File: "signalled_mount.json",
			Inputs: spechash.Inputs{
				Revisions: map[string]string{"tls-cert": "rev-one"},
				Refreshed: []manifest.Reference{{Kind: manifest.KindSecret, Name: "tls-cert"}},
			},
		},
		{
			Name:   "references another workload",
			File:   "references_workload.json",
			Inputs: spechash.Inputs{Addresses: map[string]string{"workload:api:http": "10.0.0.1:20000"}},
		},
		{
			Name:   "pulls the image on every start",
			File:   "pull_always.json",
			Inputs: spechash.Inputs{Digest: "sha256:abc"},
		},
		{
			Name: "runs a command on the host",
			File: "exec.json",
		},
		{
			// The count is part of the specification, so scaling a workload moves
			// its hash and replaces its instances through the ordinary stale path.
			Name: "runs more than one instance",
			File: "count.json",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			spec := readSpec(t, tc.File)

			encoded, hash, err := spechash.Compute(spec, tc.Inputs)
			require.NoError(t, err)

			golden.Assert(t, hash, goldenFor(tc.File))

			// The bytes that come back are the specification alone. Nothing about a
			// secret is written to the database or echoed back by the API.
			assert.JSONEq(t, string(readFixture(t, tc.File)), string(encoded))
		})
	}
}

// TestCompute_CountOfOneIsTheDefault holds the rule that writing count: 1 asks for
// what leaving it out already means. The two must hash identically, or adding the
// explicit form to a manifest would replace a workload that is not changing.
func TestCompute_CountOfOneIsTheDefault(t *testing.T) {
	t.Parallel()

	counted, err := manifest.Parse(strings.NewReader(
		"version: v1\nname: example\ncount: 1\ncontainer:\n  image: example/example:latest\n"))
	require.NoError(t, err)

	uncounted, err := manifest.Parse(strings.NewReader(
		"version: v1\nname: example\ncontainer:\n  image: example/example:latest\n"))
	require.NoError(t, err)

	_, explicit, err := spechash.Compute(counted, spechash.Inputs{})
	require.NoError(t, err)

	_, defaulted, err := spechash.Compute(uncounted, spechash.Inputs{})
	require.NoError(t, err)

	assert.Equal(t, explicit, defaulted)
}

// TestCompute_SignallingMountIsNotASpecificationChange holds the rule the omitempty on
// hashedSpec's maps exists for.
func TestCompute_SignallingMountIsNotASpecificationChange(t *testing.T) {
	t.Parallel()

	spec := readSpec(t, "signalled_mount.json")

	_, signalled, err := spechash.Compute(spec, spechash.Inputs{
		Revisions: map[string]string{"tls-cert": "rev-one"},
		Refreshed: []manifest.Reference{{Kind: manifest.KindSecret, Name: "tls-cert"}},
	})
	require.NoError(t, err)

	_, rotated, err := spechash.Compute(spec, spechash.Inputs{
		Revisions: map[string]string{"tls-cert": "rev-two"},
		Refreshed: []manifest.Reference{{Kind: manifest.KindSecret, Name: "tls-cert"}},
	})
	require.NoError(t, err)

	// Naming a signal asks for the file to be rewritten and the workload signalled. A
	// hash that moved with the value would have the reconciler replace the instance
	// instead, which is the one thing the mount asked not to happen.
	assert.Equal(t, signalled, rotated)

	_, unread, err := spechash.Compute(spec, spechash.Inputs{})
	require.NoError(t, err)

	// And it hashes as a workload reading nothing, because an empty map is left out
	// rather than written as null.
	assert.Equal(t, unread, signalled)
}

// goldenFor names the golden file holding the hash of the given fixture.
func goldenFor(file string) string {
	return strings.TrimSuffix(file, ".json") + ".golden"
}

func readFixture(t *testing.T, file string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", file))
	require.NoError(t, err)

	return data
}

func readSpec(t *testing.T, file string) manifest.Spec {
	t.Helper()

	var spec manifest.Spec
	require.NoError(t, json.Unmarshal(readFixture(t, file), &spec))

	return spec
}
