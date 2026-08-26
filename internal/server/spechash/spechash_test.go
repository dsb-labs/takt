package spechash_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/spechash"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// TestCompute pins a known specification to a known hash.
//
// The hash decides whether a running instance is destroyed and replaced, so a failure
// here means every workload on every node is about to be replaced on upgrade. Treat it
// as a change to be justified rather than a fixture to be refreshed.
func TestCompute(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name   string
		File   string
		Inputs spechash.Inputs
		Hash   string
	}{
		{
			// The case every other one is measured against. A workload reading
			// nothing hashes over its specification alone, with no wrapper, which is
			// what stops an upgrade replacing every running instance.
			Name: "reads nothing",
			File: "plain.json",
			Hash: "dfcc34c199e930259a837197da02c7f856ff32333f13ed6808155190c5242379",
		},
		{
			Name:   "reads only secrets",
			File:   "reads_secret.json",
			Inputs: spechash.Inputs{Revisions: map[string]string{"db-password": "rev-one"}},
			Hash:   "b0296668cd84a55b00e0586a229fa2487f1c343d7905483de65359a5ce1efd91",
		},
		{
			Name:   "reads only variables",
			File:   "reads_variable.json",
			Inputs: spechash.Inputs{Values: map[string]string{"region": "eu-west-1"}},
			Hash:   "df29a19d3ea89d05bdf940c5361267fd126aaa7dd237395ae5350b1d5354efe0",
		},
		{
			Name: "reads both",
			File: "reads_both.json",
			Inputs: spechash.Inputs{
				Revisions: map[string]string{"db-password": "rev-one"},
				Values:    map[string]string{"region": "eu-west-1"},
			},
			Hash: "c1ff757fdd20804ad9e6a35eb1f65c3f7c9b28121042b69de24bfada7f4278eb",
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
			Hash: "0af39cd3b19c414e3f8e94929a007467193d2feaa0c45d76130ff173b202a395",
		},
		{
			Name:   "references another workload",
			File:   "references_workload.json",
			Inputs: spechash.Inputs{Addresses: map[string]string{"workload:api:http": "10.0.0.1:20000"}},
			Hash:   "9e1dfe46e1e3dd2345e2bb4abcbd90b0d39c3c80aff6ce26e96b2eb8a4e42a6f",
		},
		{
			Name:   "pulls the image on every start",
			File:   "pull_always.json",
			Inputs: spechash.Inputs{Digest: "sha256:abc"},
			Hash:   "5b2743c528355106741ed2d27f48848524169fd748461bb49d802a797ddc6639",
		},
		{
			Name: "runs a command on the host",
			File: "exec.json",
			Hash: "0fe1e3c64790597e702e2545b50292c7558fe03f8dc86a56ed6b84eac20121da",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			spec := readSpec(t, tc.File)

			encoded, hash, err := spechash.Compute(spec, tc.Inputs)
			require.NoError(t, err)

			assert.Equal(t, tc.Hash, hash)

			// The bytes that come back are the specification alone. Nothing about a
			// secret is written to the database or echoed back by the API.
			assert.JSONEq(t, string(readFixture(t, tc.File)), string(encoded))
		})
	}
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
