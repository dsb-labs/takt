package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Assert       func(*testing.T, manifest.Spec)
		ExpectErr    error
		ExpectsError bool
	}{
		{
			Name: "a full container manifest",
			File: "container.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, "v1", spec.Version)
				assert.Equal(t, "example", spec.Name)

				assert.Equal(t, "*/5 * * * *", spec.Schedule)
				assert.Equal(t, map[string]string{"some-key": "some-value"}, spec.Labels)

				require.NotNil(t, spec.Container)
				assert.Equal(t, "example/example:latest", spec.Container.Image)
				assert.Equal(t, map[string]string{"EXAMPLE": "EXAMPLE"}, spec.Container.Env)
				assert.Equal(t, []manifest.Port{{To: 8080, From: 4141}, {To: 9090}}, spec.Container.Ports)

				assert.Nil(t, spec.Script)
			},
		},
		{
			Name: "a minimal container manifest",
			File: "minimal.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Container)
				assert.Equal(t, "example/example:latest", spec.Container.Image)
				assert.Empty(t, spec.Schedule)
				assert.Empty(t, spec.Container.Ports)
			},
		},
		{
			Name: "a script manifest with a raw body",
			File: "script_raw.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Script)
				assert.Equal(t, `echo "hello world"`, spec.Script.Raw)
				assert.Nil(t, spec.Container)
			},
		},
		{
			Name: "a script manifest with a source url",
			File: "script_source.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Script)
				assert.Equal(t, "https://example.com/some_script.sh", spec.Script.Source)
			},
		},
		{
			Name: "a health check over http",
			File: "health_http.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Health)
				assert.Equal(t, "/healthz", spec.Health.HTTP)
				assert.Equal(t, 5*time.Second, spec.Health.Interval)
				assert.Equal(t, time.Second, spec.Health.Timeout)
				assert.Equal(t, 2, spec.Health.Retries)
				assert.Equal(t, 15*time.Second, spec.Health.StartPeriod)
			},
		},
		{
			Name: "a health check over tcp takes the timing defaults",
			File: "health_tcp.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Health)
				assert.True(t, spec.Health.TCP)

				// Resolved here so nothing downstream has to decide what an unset
				// interval means.
				assert.Equal(t, manifest.DefaultHealthInterval, spec.Health.Interval)
				assert.Equal(t, manifest.DefaultHealthTimeout, spec.Health.Timeout)
				assert.Equal(t, manifest.DefaultHealthRetries, spec.Health.Retries)
				assert.Equal(t, manifest.DefaultHealthStartPeriod, spec.Health.StartPeriod)
			},
		},
		{
			Name:         "rejects a health check naming no probe",
			File:         "health_no_probe.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a health check naming both probes",
			File:         "health_both_probes.yaml",
			ExpectsError: true,
		},
		{
			Name: "rejects a health check on a workload publishing no ports",
			File: "health_no_ports.yaml",
			// A probe is performed against a published address, so there would be
			// nothing to check and the workload would never report healthy.
			ExpectsError: true,
		},
		{
			Name: "rejects an ambiguous port when several are published",
			File: "health_ambiguous_port.yaml",
			// Guessing which port to check would make the manifest mean something
			// the operator did not say.
			ExpectsError: true,
		},
		{
			Name:         "rejects a port the workload does not publish",
			File:         "health_unknown_port.yaml",
			ExpectsError: true,
		},
		{
			Name: "rejects a probe the runtime cannot perform",
			File: "health_script.yaml",
			// A script runs to completion and has no address to probe, so this is
			// rejected rather than left to sit as starting forever.
			ExpectsError: true,
		},
		{
			Name:         "rejects a timing field that is not a duration",
			File:         "health_bad_duration.yaml",
			ExpectsError: true,
		},
		{
			Name: "rejects a timeout longer than the interval",
			File: "health_timeout_over_interval.yaml",
			// Checks would overlap, so a failure count would stop meaning
			// consecutive failures.
			ExpectsError: true,
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
			// A policy nobody recognises is a typo, and defaulting it silently would
			// leave a job the operator meant to run once running forever.
			Name:         "rejects an unknown restart policy",
			File:         "bad_restart.yaml",
			ExpectsError: true,
		},
		{
			Name: "a manifest asking for an allocated host port",
			File: "dynamic_ports.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.Len(t, spec.Container.Ports, 1)
				assert.Equal(t, 8080, spec.Container.Ports[0].To)

				// An unset host port is what asks the server to allocate one.
				assert.Zero(t, spec.Container.Ports[0].From)
			},
		},
		{
			Name:         "rejects a port outside the usable range",
			File:         "bad_ports.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects the same container port published twice",
			File:         "duplicate_ports.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects the same host port used twice",
			File:         "duplicate_host_ports.yaml",
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

func TestRuntimeOf(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Spec      manifest.Spec
		Expected  manifest.Runtime
		ExpectErr error
	}{
		{
			Name:     "a container block selects the container runtime",
			Spec:     manifest.Spec{Container: &manifest.Container{Image: "example/example:latest"}},
			Expected: manifest.RuntimeContainer,
		},
		{
			Name:     "a script block selects the script runtime",
			Spec:     manifest.Spec{Script: &manifest.Script{Raw: "echo hello"}},
			Expected: manifest.RuntimeScript,
		},
		{
			Name:      "no block at all",
			Spec:      manifest.Spec{},
			ExpectErr: manifest.ErrNoRuntime,
		},
		{
			Name: "two blocks",
			Spec: manifest.Spec{
				Container: &manifest.Container{Image: "example/example:latest"},
				Script:    &manifest.Script{Raw: "echo hello"},
			},
			ExpectErr: manifest.ErrAmbiguousRuntime,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := manifest.RuntimeOf(tc.Spec)
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
func TestRestartPolicy_Restarts(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name           string
		Policy         manifest.RestartPolicy
		ExitCode       int
		ExpectRestarts bool
	}{
		{
			Name:           "always restarts a clean exit",
			Policy:         manifest.RestartAlways,
			ExpectRestarts: true,
		},
		{
			Name:           "always restarts a failure",
			Policy:         manifest.RestartAlways,
			ExitCode:       1,
			ExpectRestarts: true,
		},
		{
			// The workload did what it was asked to do, which is the whole point of
			// this policy: a job that finishes is finished.
			Name:           "on-failure leaves a clean exit alone",
			Policy:         manifest.RestartOnFailure,
			ExpectRestarts: false,
		},
		{
			Name:           "on-failure restarts a failure",
			Policy:         manifest.RestartOnFailure,
			ExitCode:       1,
			ExpectRestarts: true,
		},
		{
			Name:           "never leaves a clean exit alone",
			Policy:         manifest.RestartNever,
			ExpectRestarts: false,
		},
		{
			Name:           "never leaves a failure alone",
			Policy:         manifest.RestartNever,
			ExitCode:       137,
			ExpectRestarts: false,
		},
		{
			// Validation rejects an unknown policy, so reaching this means the stored
			// specification and the rules have diverged. Restarting is the better
			// failure: a service kept running beats one silently retired.
			Name:           "an unrecognised policy restarts",
			Policy:         "sometimes",
			ExpectRestarts: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.Equal(t, tc.ExpectRestarts, tc.Policy.Restarts(tc.ExitCode))
		})
	}
}

func TestRestartPolicy_Default(t *testing.T) {
	t.Parallel()

	// A manifest reaches the server two ways, decoded from YAML and converted from
	// the wire. A default applied to only one of them would make the same manifest
	// behave differently depending on the route it took.
	t.Run("parsing a manifest that says nothing", func(t *testing.T) {
		spec, err := manifest.Parse(strings.NewReader(`
version: v1
name: example
container:
  image: example/example:latest
`))
		require.NoError(t, err)
		assert.Equal(t, manifest.RestartAlways, spec.Restart)
	})

	t.Run("converting a wire specification that says nothing", func(t *testing.T) {
		spec := manifest.NewSpec(api.WorkloadSpec{
			Version:   "v1",
			Name:      "example",
			Container: &api.ContainerSpec{Image: "example/example:latest"},
		})

		assert.Equal(t, manifest.RestartAlways, spec.Restart)
	})

	t.Run("a policy the manifest states is kept", func(t *testing.T) {
		spec, err := manifest.Parse(strings.NewReader(`
version: v1
name: example
restart: never
container:
  image: example/example:latest
`))
		require.NoError(t, err)
		assert.Equal(t, manifest.RestartNever, spec.Restart)
	})
}

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
	assert.NotEmpty(t, spec.Schedule)
	assert.NotEmpty(t, spec.Restart)
	assert.NotEmpty(t, spec.Labels)
	require.NotNil(t, spec.Container)
	assert.NotEmpty(t, spec.Container.Image)
	assert.NotEmpty(t, spec.Container.Env)
	assert.NotEmpty(t, spec.Container.Ports)
}
