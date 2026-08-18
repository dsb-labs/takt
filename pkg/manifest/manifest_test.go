package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Assert       func(*testing.T, api.WorkloadSpec)
		ExpectErr    error
		ExpectsError bool
	}{
		{
			Name: "a full container manifest",
			File: "container.yaml",
			Assert: func(t *testing.T, spec api.WorkloadSpec) {
				assert.Equal(t, "v1", spec.Version)
				assert.Equal(t, "example", spec.Name)

				require.NotNil(t, spec.Schedule)
				assert.Equal(t, "*/5 * * * *", *spec.Schedule)

				require.NotNil(t, spec.Labels)
				assert.Equal(t, map[string]string{"some-key": "some-value"}, *spec.Labels)

				require.NotNil(t, spec.Container)
				assert.Equal(t, "example/example:latest", spec.Container.Image)

				require.NotNil(t, spec.Container.Env)
				assert.Equal(t, map[string]string{"EXAMPLE": "EXAMPLE"}, *spec.Container.Env)

				require.NotNil(t, spec.Container.Ports)
				assert.Equal(t, []string{"8080:8080"}, *spec.Container.Ports)

				assert.Nil(t, spec.Script)
			},
		},
		{
			Name: "a minimal container manifest",
			File: "minimal.yaml",
			Assert: func(t *testing.T, spec api.WorkloadSpec) {
				require.NotNil(t, spec.Container)
				assert.Equal(t, "example/example:latest", spec.Container.Image)
				assert.Nil(t, spec.Schedule)
				assert.Nil(t, spec.Container.Ports)
			},
		},
		{
			Name: "a script manifest with a raw body",
			File: "script_raw.yaml",
			Assert: func(t *testing.T, spec api.WorkloadSpec) {
				require.NotNil(t, spec.Script)
				require.NotNil(t, spec.Script.Raw)
				assert.Equal(t, `echo "hello world"`, *spec.Script.Raw)
				assert.Nil(t, spec.Container)
			},
		},
		{
			Name: "a script manifest with a source url",
			File: "script_source.yaml",
			Assert: func(t *testing.T, spec api.WorkloadSpec) {
				require.NotNil(t, spec.Script)
				require.NotNil(t, spec.Script.Source)
				assert.Equal(t, "https://example.com/some_script.sh", *spec.Script.Source)
			},
		},
		{
			Name:      "rejects a manifest naming no runtime",
			File:      "no_runtime.yaml",
			ExpectErr: manifest.ErrNoRuntime,
		},
		{
			Name:      "rejects a manifest naming two runtimes",
			File:      "two_runtimes.yaml",
			ExpectErr: manifest.ErrAmbiguousRuntime,
		},
		{
			Name:         "rejects a script naming both source and raw",
			File:         "script_both.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a script naming neither source nor raw",
			File:         "script_neither.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects an unknown schema version",
			File:         "bad_version.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a name that isn't a dns label",
			File:         "bad_name.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a schedule that isn't cron",
			File:         "bad_schedule.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects port mappings the driver couldn't parse",
			File:         "bad_ports.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a container with no image",
			File:         "no_image.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects an unknown field",
			File:         "unknown_field.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects malformed yaml",
			File:         "malformed.yaml",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.File))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })

			spec, err := manifest.Parse(f)
			switch {
			case tc.ExpectErr != nil:
				assert.ErrorIs(t, err, tc.ExpectErr)
				assert.Zero(t, spec)
				return
			case tc.ExpectsError:
				assert.Error(t, err)
				assert.Zero(t, spec)
				return
			}

			require.NoError(t, err)
			tc.Assert(t, spec)
		})
	}
}

func TestRuntime(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Spec      api.WorkloadSpec
		Expected  api.Runtime
		ExpectErr error
	}{
		{
			Name:     "a container block selects the container runtime",
			Spec:     api.WorkloadSpec{Container: &api.ContainerSpec{Image: "example/example:latest"}},
			Expected: api.Container,
		},
		{
			Name:     "a script block selects the script runtime",
			Spec:     api.WorkloadSpec{Script: &api.ScriptSpec{Raw: new("echo hello")}},
			Expected: api.Script,
		},
		{
			Name:      "no block at all",
			Spec:      api.WorkloadSpec{},
			ExpectErr: manifest.ErrNoRuntime,
		},
		{
			Name: "two blocks",
			Spec: api.WorkloadSpec{
				Container: &api.ContainerSpec{Image: "example/example:latest"},
				Script:    &api.ScriptSpec{Raw: new("echo hello")},
			},
			ExpectErr: manifest.ErrAmbiguousRuntime,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := manifest.Runtime(tc.Spec)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, got)
		})
	}
}

// TestParse_EveryFieldDecodes pins the YAML-to-Go field mapping that Parse relies
// on. The generated types carry only JSON tags and yaml.v3 matches keys against
// the lowercased Go field name, which works only while every manifest key is a
// single word. Adding a multi-word field to the specification — spelled camelCase
// in JSON — would fail to decode here, and this test is what catches it.
func TestParse_EveryFieldDecodes(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join("testdata", "container.yaml"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })

	spec, err := manifest.Parse(f)
	require.NoError(t, err)

	// Every field the manifest schema declares must have survived the round trip.
	// A nil here means a key in the fixture didn't map onto its Go field.
	assert.NotEmpty(t, spec.Version)
	assert.NotEmpty(t, spec.Name)
	require.NotNil(t, spec.Schedule)
	assert.NotEmpty(t, *spec.Schedule)
	require.NotNil(t, spec.Labels)
	assert.NotEmpty(t, *spec.Labels)
	require.NotNil(t, spec.Container)
	assert.NotEmpty(t, spec.Container.Image)
	require.NotNil(t, spec.Container.Env)
	assert.NotEmpty(t, *spec.Container.Env)
	require.NotNil(t, spec.Container.Ports)
	assert.NotEmpty(t, *spec.Container.Ports)
}
