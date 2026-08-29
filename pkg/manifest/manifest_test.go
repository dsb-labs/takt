package manifest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

				require.NotNil(t, spec.Schedule)
				assert.Equal(t, "*/5 * * * *", spec.Schedule.Cron)
				assert.Equal(t, manifest.OverlapSkip, spec.Schedule.Overlap)

				assert.Equal(t, map[string]string{"some-key": "some-value"}, spec.Labels)

				require.NotNil(t, spec.Container)
				assert.Equal(t, "example/example:latest", spec.Container.Image)
				assert.Equal(t, map[string]string{"EXAMPLE": "EXAMPLE"}, spec.Env)
				assert.Equal(t, []manifest.Port{
					{Name: "http", To: 8080, From: 4141, Protocol: manifest.ProtocolTCP},
					{To: 9090, Protocol: manifest.ProtocolTCP},
				}, spec.Ports)
				assert.Equal(t, []manifest.VolumeMount{{Name: "example-data", To: "/var/lib/example"}}, spec.Volumes)

				assert.Nil(t, spec.Exec)
			},
		},
		{
			Name: "a minimal container manifest",
			File: "minimal.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Container)
				assert.Equal(t, "example/example:latest", spec.Container.Image)
				assert.Empty(t, spec.Schedule)
				assert.Empty(t, spec.Ports)
			},
		},
		{
			Name: "an exec manifest",
			File: "exec.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Exec)
				assert.Equal(t, []string{"/usr/local/bin/backup", "--target", "/data"}, spec.Exec.Command)
				assert.Equal(t, map[string]string{"EXAMPLE": "EXAMPLE"}, spec.Env)
				assert.Nil(t, spec.Container)
			},
		},
		{
			// An exec process binds a host port itself, so the port it binds has to be
			// named. The server records it rather than choosing it.
			Name: "an exec manifest pinning the host port it binds",
			File: "exec_ports_pinned.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.Len(t, spec.Ports, 1)
				assert.Equal(t, 8080, spec.Ports[0].From)
			},
		},
		{
			// Ports and a check are both meaningful for a long-running exec workload,
			// which listens on the host the same way a container listens in its
			// namespace.
			Name: "an exec manifest with a health check",
			File: "exec_health.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Health)
				assert.Equal(t, "/healthz", spec.Health.HTTP)
			},
		},
		{
			Name:         "rejects an exec manifest naming no command",
			File:         "exec_no_command.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects an exec command with an empty element",
			File:         "exec_empty_command.yaml",
			ExpectsError: true,
		},
		{
			// The server allocates nothing for exec, so a port with no host port named
			// would leave the process with nothing to bind.
			Name:         "rejects exec ports that do not name a host port",
			File:         "exec_ports_unpinned.yaml",
			ExpectsError: true,
		},
		{
			// A check is performed against an address, so a workload publishing none
			// cannot be probed whatever its runtime.
			Name:         "rejects a check on a workload that publishes no ports",
			File:         "exec_health_no_ports.yaml",
			ExpectsError: true,
		},
		{
			// A connection to a UDP port always succeeds, so a check against one
			// would report every workload as healthy.
			Name:         "rejects a check on a workload publishing only udp",
			File:         "health_udp_only.yaml",
			ExpectsError: true,
		},
		{
			// Only the TCP port is checkable, so naming one is not ambiguous even
			// though two ports are published.
			Name: "a check on the tcp side of a dual-protocol workload",
			File: "health_dual_ports.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Health)
				assert.True(t, spec.Health.TCP)
				assert.Zero(t, spec.Health.Port)
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
			Name: "a health check naming the port by name",
			File: "health_named_port.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Health)
				assert.Equal(t, manifest.PortRef("http"), spec.Health.Port)
			},
		},
		{
			// The number is the other way of writing the same thing, and a manifest
			// naming it should not have to quote it.
			Name: "a health check naming the port by number",
			File: "health_numbered_port.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Health)
				assert.Equal(t, manifest.PortRef("8080"), spec.Health.Port)
			},
		},
		{
			Name:         "rejects a port name the workload does not publish",
			File:         "health_unknown_name.yaml",
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
			Name: "accepts the label convention operators arrive with",
			File: "labels_dotted_keys.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, map[string]string{
					"app.kubernetes.io/name": "web",
					"env_tier":               "prod",
					"a":                      "b",
					"note":                   "",
				}, spec.Labels)
			},
		},
		{
			// Keys are held to a lowercase printable shape so they survive container
			// metadata and the list query filter.
			Name:         "rejects a label key outside the documented shape",
			File:         "labels_bad_key.yaml",
			ExpectsError: true,
		},
		{
			// The driver writes its own labels after copying these, so refusal is
			// feedback — a silently overwritten value would vanish with no explanation.
			Name:         "rejects a label key using the reserved orca. prefix",
			File:         "labels_reserved.yaml",
			ExpectsError: true,
		},
		{
			// The value reaches container metadata and terminal output, where a control
			// character breaks whatever is displaying it.
			Name:         "rejects a label value holding a control character",
			File:         "labels_control_value.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a manifest carrying more than the maximum labels",
			File:         "labels_too_many.yaml",
			ExpectsError: true,
		},
		{
			// The decoder refuses duplicate mapping keys, so validation never sees a
			// manifest where the same label appears twice. This pins that behaviour.
			Name:         "rejects a label key defined twice",
			File:         "labels_duplicate_key.yaml",
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
			Name:         "rejects an unknown overlap policy",
			File:         "bad_overlap.yaml",
			ExpectsError: true,
		},
		{
			// A pull policy nobody recognises is a typo, and defaulting it silently
			// would pin an image the operator asked to have refreshed.
			Name:         "rejects an unknown pull policy",
			File:         "bad_pull.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a restart delay that is not a duration",
			File:         "bad_delay.yaml",
			ExpectsError: true,
		},
		{
			// A schedule with no expression names no times, so there is nothing for
			// orca to act on.
			Name:         "rejects a schedule naming no expression",
			File:         "no_cron.yaml",
			ExpectsError: true,
		},
		{
			// A check restarts a workload that stops answering, and a scheduled
			// workload is expected to end. Together the check would fight the
			// schedule.
			Name:         "rejects a schedule alongside a health check",
			File:         "schedule_health.yaml",
			ExpectsError: true,
		},
		{
			// An empty element reaches the runtime as an empty argument, which is
			// either meaningless or means something the operator did not write.
			Name:         "rejects a command with an empty element",
			File:         "bad_command.yaml",
			ExpectsError: true,
		},
		{
			Name: "a manifest asking for an allocated host port",
			File: "dynamic_ports.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.Len(t, spec.Ports, 1)
				assert.Equal(t, 8080, spec.Ports[0].To)

				// An unset host port is what asks the server to allocate one.
				assert.Zero(t, spec.Ports[0].From)
			},
		},
		{
			Name: "a manifest naming its ports",
			File: "named_ports.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, []manifest.Port{
					{Name: "http", To: 8080, Protocol: manifest.ProtocolTCP},
					{Name: "dns", To: 53, Protocol: manifest.ProtocolTCP},
					{Name: "dns", To: 53, Protocol: manifest.ProtocolUDP},
				}, spec.Ports)
			},
		},
		{
			Name:         "rejects a port name that is not a usable name",
			File:         "bad_port_name.yaml",
			ExpectsError: true,
		},
		{
			// A port is also named by the port itself, so a name that reads as a
			// number would be two different ports written the same way.
			Name:         "rejects a port named as a number",
			File:         "port_name_numeric.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects one name given to two different ports",
			File:         "port_name_reused.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a port outside the usable range",
			File:         "bad_ports.yaml",
			ExpectsError: true,
		},
		{
			Name: "a udp port",
			File: "ports_udp.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, []manifest.Port{{To: 51820, Protocol: manifest.ProtocolUDP}}, spec.Ports)
			},
		},
		{
			// TCP and UDP are separate address spaces, so one workload publishing the
			// same number on both is publishing two different ports, which is what
			// DNS wants.
			Name: "the same port published on both protocols",
			File: "ports_dual.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, []manifest.Port{
					{To: 53, Protocol: manifest.ProtocolTCP},
					{To: 53, Protocol: manifest.ProtocolUDP},
				}, spec.Ports)
			},
		},
		{
			Name:         "rejects a protocol orca cannot publish",
			File:         "ports_bad_protocol.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects the same container port published twice",
			File:         "duplicate_ports.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects the same udp port published twice",
			File:         "duplicate_udp_ports.yaml",
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
			// The same field, written the same way, for the other runtime. What it
			// resolves to differs. What a manifest may say does not.
			Name: "mounts a volume in an exec workload",
			File: "volumes_exec.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Exec)
				assert.Equal(t, []manifest.VolumeMount{{Name: "example-data", To: "/var/lib/example"}}, spec.Volumes)
			},
		},
		{
			// A relative path has no meaning to a container runtime, so the field
			// means one thing rather than two.
			Name:         "rejects a relative mount path",
			File:         "volumes_relative.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a volume mounted at the root",
			File:         "volumes_root.yaml",
			ExpectsError: true,
		},
		{
			// Cleaned before comparing, so a trailing slash does not hide a clash.
			Name:         "rejects two volumes mounted at the same path",
			File:         "volumes_duplicate_path.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects the same volume mounted twice",
			File:         "volumes_duplicate_name.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a volume name orca would not accept",
			File:         "volumes_bad_name.yaml",
			ExpectsError: true,
		},
		{
			// The three sources in one list, which is the point of putting them there:
			// a workload says what appears in its filesystem in one place, however the
			// contents are produced.
			Name: "mounts a volume, a secret and a variable",
			File: "mounts_values.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, []manifest.VolumeMount{
					{Name: "example-data", To: "/var/lib/example"},
					{Secret: "secret-name", To: "/var/secret.json"},
					{Var: "variable-name", To: "/var/example.json"},
				}, spec.Volumes)
			},
		},
		{
			Name: "mounts values that ask to be signalled",
			File: "mounts_signal.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, []manifest.VolumeMount{
					{Secret: "tls-cert", To: "/etc/tls/cert.pem", Signal: manifest.SignalHUP},
					{Var: "app-config", To: "/etc/app/config.json", Signal: manifest.SignalUSR1},
				}, spec.Volumes)
			},
		},
		{
			// A secret and a variable may share a name and hold different values, so
			// mounting both is two mounts rather than a duplicate.
			Name: "mounts a secret and a variable sharing a name",
			File: "mounts_same_name.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, []manifest.VolumeMount{
					{Secret: "shared-name", To: "/var/secret.json"},
					{Var: "shared-name", To: "/var/variable.json"},
				}, spec.Volumes)
			},
		},
		{
			Name:      "rejects a mount naming nothing to mount",
			File:      "mounts_no_source.yaml",
			ExpectErr: manifest.ErrNoMountSource,
		},
		{
			Name:      "rejects a mount naming two things to mount",
			File:      "mounts_two_sources.yaml",
			ExpectErr: manifest.ErrAmbiguousMountSource,
		},
		{
			// orca does not know what a workload writes into a volume, so a signal there
			// asks for something that would never happen.
			Name:         "rejects a signal on a mounted volume",
			File:         "mounts_volume_signal.yaml",
			ExpectsError: true,
		},
		{
			// Whether a workload runs is the reconciler's decision, so a manifest that
			// could stop one would be taking it.
			Name:         "rejects a signal that would stop the workload",
			File:         "mounts_bad_signal.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects the same secret mounted twice",
			File:         "mounts_duplicate_secret.yaml",
			ExpectsError: true,
		},
		{
			Name: "keeps a secret reference as written",
			File: "env_secrets.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				// Parsing leaves the reference alone. Only the server holds the key, so
				// the text is what travels and what is stored.
				assert.Equal(t, "${secret:secret-name}", spec.Env["EXAMPLE"])
				assert.Equal(t, "postgres://app:${secret:db-password}@localhost:5432/app", spec.Env["DSN"])
				assert.Equal(t, "$$notasecret", spec.Env["LITERAL"])
			},
		},
		{
			Name:         "rejects an unterminated secret reference",
			File:         "env_bad_secret.yaml",
			ExpectsError: true,
		},
		{
			Name: "keeps a variable reference as written",
			File: "env_variables.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				// Parsing leaves the reference alone, as it does for a secret. A
				// variable is held by the server, so the text is what travels.
				assert.Equal(t, "${var:variable-name}", spec.Env["EXAMPLE"])
				assert.Equal(t, "postgres://app@${var:db-host}:5432/app", spec.Env["DSN"])
				assert.Equal(t, "$$notavariable", spec.Env["LITERAL"])
			},
		},
		{
			Name: "keeps both kinds of reference as written",
			File: "env_mixed.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				assert.Equal(t, "${secret:secret-name}", spec.Env["EXAMPLE"])
				assert.Equal(t, "${var:variable-name}", spec.Env["EXAMPLE2"])
				assert.Equal(t, "postgres://app:${secret:db-password}@${var:db-host}:5432/app", spec.Env["DSN"])
			},
		},
		{
			Name:         "rejects an unterminated variable reference",
			File:         "env_bad_variable.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects malformed yaml",
			File:         "malformed.yaml",
			ExpectsError: true,
		},
		{
			Name: "a container manifest with resource limits",
			File: "resources.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Resources)
				assert.Equal(t, "512m", spec.Resources.Memory)
				assert.Equal(t, 0.5, spec.Resources.CPU)
				assert.Equal(t, 100, spec.Resources.Pids)
			},
		},
		{
			// Whether the host delegates the cgroup subtree enforcement needs is the
			// server's to answer at apply time, so the manifest is valid here.
			Name: "an exec manifest with resource limits",
			File: "resources_exec.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Resources)
				assert.Equal(t, "512m", spec.Resources.Memory)
			},
		},
		{
			// An empty block asks for nothing, which leaving the section out already
			// says.
			Name:         "rejects a resources block naming no limit",
			File:         "resources_empty.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a memory limit that is not a size",
			File:         "resources_bad_memory.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a negative cpu limit",
			File:         "resources_negative_cpu.yaml",
			ExpectsError: true,
		},
		{
			Name: "a hardened container manifest",
			File: "container_hardened.yaml",
			Assert: func(t *testing.T, spec manifest.Spec) {
				require.NotNil(t, spec.Container)
				assert.Equal(t, "65532:65532", spec.Container.User)
				assert.True(t, spec.Container.ReadOnly)
				assert.Equal(t, []string{"NET_ADMIN"}, spec.Container.CapAdd)
				assert.Equal(t, []string{"ALL"}, spec.Container.CapDrop)
			},
		},
		{
			// An empty element reaches the runtime as a capability that means nothing
			// or was not written, so it is rejected the way an empty command element is.
			Name:         "rejects an empty capability element",
			File:         "container_empty_capability.yaml",
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

func TestParseVolume(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Expected     manifest.Volume
		ExpectsError bool
	}{
		{
			Name:     "a volume manifest",
			File:     "volume.yaml",
			Expected: manifest.Volume{Version: "v1", Name: "example-data"},
		},
		{
			Name: "a volume manifest with labels",
			File: "volume_labels.yaml",
			Expected: manifest.Volume{
				Version: "v1",
				Name:    "example-data",
				Labels:  map[string]string{"app": "web", "app.kubernetes.io/name": "example"},
			},
		},
		{
			Name:         "rejects a name orca would not accept",
			File:         "volume_bad_name.yaml",
			ExpectsError: true,
		},
		{
			// A volume's labels answer to the same rules a workload's do, reserved
			// prefix included, so an operator learns them once.
			Name:         "rejects a label orca reserves for itself",
			File:         "volume_bad_label.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a schema version it does not understand",
			File:         "volume_bad_version.yaml",
			ExpectsError: true,
		},
		{
			// A volume holds data and has nothing to configure, so a key that looks
			// like configuration is a misunderstanding worth reporting.
			Name:         "rejects an unknown field",
			File:         "volume_unknown_field.yaml",
			ExpectsError: true,
		},
		{
			// Which resource a file describes is decided by what it is given to, so
			// a workload manifest handed to this reads as unknown keys.
			Name:         "rejects a workload manifest",
			File:         "container.yaml",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.File))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })

			volume, err := manifest.ParseVolume(f)
			if tc.ExpectsError {
				assert.Error(t, err)
				assert.Zero(t, volume)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, volume)
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
			Name:     "an exec block selects the exec runtime",
			Spec:     manifest.Spec{Exec: &manifest.Exec{Command: []string{"echo", "hello"}}},
			Expected: manifest.RuntimeExec,
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
				Exec:      &manifest.Exec{Command: []string{"echo", "hello"}},
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

// TestValidate_Labels pins the label boundaries that are unwieldy as fixtures:
// the exact length caps, the byte counting on values, and the printable
// non-ASCII cases.
func TestValidate_Labels(t *testing.T) {
	t.Parallel()

	spec := func(labels map[string]string) manifest.Spec {
		return manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Labels:    labels,
			Container: &manifest.Container{Image: "example/example:latest"},
		}
	}

	t.Run("rejects keys outside the documented shape", func(t *testing.T) {
		t.Parallel()

		for _, key := range []string{
			strings.Repeat("a", 64),
			"",
			"-leading",
			"trailing.",
			"has space",
			"orca.workload",
		} {
			err := manifest.Validate(spec(map[string]string{key: "value"}))
			assert.Error(t, err, "accepted the key %q", key)
		}
	})

	t.Run("rejects values outside the documented shape", func(t *testing.T) {
		t.Parallel()

		for _, value := range []string{
			strings.Repeat("v", 257),
			// 129 two-byte runes. The cap counts bytes, so 258 bytes is over
			// the limit even though it reads as 129 characters.
			strings.Repeat("é", 129),
			"a\tb",
			string([]byte{0xff}),
		} {
			err := manifest.Validate(spec(map[string]string{"some-key": value}))
			assert.Error(t, err, "accepted the value %q", value)
		}
	})

	t.Run("accepts labels at the boundaries", func(t *testing.T) {
		t.Parallel()

		for name, labels := range map[string]map[string]string{
			"a key at the length cap":       {strings.Repeat("a", 63): "value"},
			"a value at the length cap":     {"some-key": strings.Repeat("v", 256)},
			"the bare key orca":             {"orca": "value"},
			"printable non-ascii in values": {"some-key": "café ☕"},
			"an empty value":                {"some-key": ""},
		} {
			err := manifest.Validate(spec(labels))
			assert.NoError(t, err, "rejected %s", name)
		}
	})

	t.Run("accepts exactly the maximum labels", func(t *testing.T) {
		t.Parallel()

		labels := make(map[string]string, 32)
		for i := range 32 {
			labels[fmt.Sprintf("key-%d", i)] = "value"
		}

		assert.NoError(t, manifest.Validate(spec(labels)))
	})
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

func TestRestart_Restarts(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name           string
		Restart        manifest.Restart
		ExitCode       int
		Attempts       int
		ExpectRestarts bool
	}{
		{
			Name:           "always restarts however many times it has tried",
			Restart:        manifest.Restart{Policy: manifest.RestartAlways},
			Attempts:       100,
			ExpectRestarts: true,
		},
		{
			// The workload did what it was asked to do, which is the whole point of
			// this policy: a job that finishes is finished.
			Name:           "on-failure leaves a clean exit alone",
			Restart:        manifest.Restart{Policy: manifest.RestartOnFailure},
			ExpectRestarts: false,
		},
		{
			Name:           "on-failure restarts a failure",
			Restart:        manifest.Restart{Policy: manifest.RestartOnFailure},
			ExitCode:       1,
			ExpectRestarts: true,
		},
		{
			Name:           "never leaves a failure alone",
			Restart:        manifest.Restart{Policy: manifest.RestartNever},
			ExitCode:       137,
			ExpectRestarts: false,
		},
		{
			Name:           "attempts not yet exhausted still restarts",
			Restart:        manifest.Restart{Policy: manifest.RestartAlways, Attempts: 3},
			Attempts:       2,
			ExpectRestarts: true,
		},
		{
			// A workload told to give up gives up, whatever its policy would
			// otherwise say.
			Name:           "attempts exhausted gives up",
			Restart:        manifest.Restart{Policy: manifest.RestartAlways, Attempts: 3},
			Attempts:       3,
			ExpectRestarts: false,
		},
		{
			Name:           "unset attempts never gives up",
			Restart:        manifest.Restart{Policy: manifest.RestartAlways},
			Attempts:       1000,
			ExpectRestarts: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.Equal(t, tc.ExpectRestarts, tc.Restart.Restarts(tc.ExitCode, tc.Attempts))
		})
	}
}

func TestRestart_Defaults(t *testing.T) {
	t.Parallel()

	t.Run("parsing a manifest that says nothing", func(t *testing.T) {
		spec, err := manifest.Parse(strings.NewReader(`
version: v1
name: example
container:
  image: example/example:latest
`))
		require.NoError(t, err)
		require.NotNil(t, spec.Restart)
		assert.Equal(t, manifest.RestartAlways, spec.Restart.Policy)
		assert.Equal(t, manifest.DefaultRestartDelay, spec.Restart.Delay)
		assert.Zero(t, spec.Restart.Attempts, "unset attempts means orca keeps trying")
	})

	t.Run("what the manifest states is kept", func(t *testing.T) {
		spec, err := manifest.Parse(strings.NewReader(`
version: v1
name: example
restart:
  policy: never
  attempts: 5
  delay: 30s
container:
  image: example/example:latest
`))
		require.NoError(t, err)
		require.NotNil(t, spec.Restart)
		assert.Equal(t, manifest.RestartNever, spec.Restart.Policy)
		assert.Equal(t, 5, spec.Restart.Attempts)
		assert.Equal(t, 30*time.Second, spec.Restart.Delay)
	})
}

func TestSchedule_Defaults(t *testing.T) {
	t.Parallel()

	t.Run("overlap defaults to replace", func(t *testing.T) {
		spec, err := manifest.Parse(strings.NewReader(`
version: v1
name: example
schedule:
  cron: "*/5 * * * *"
container:
  image: example/example:latest
`))
		require.NoError(t, err)
		require.NotNil(t, spec.Schedule)
		assert.Equal(t, manifest.OverlapReplace, spec.Schedule.Overlap)
	})

	t.Run("no schedule means the workload runs continuously", func(t *testing.T) {
		spec, err := manifest.Parse(strings.NewReader(`
version: v1
name: example
container:
  image: example/example:latest
`))
		require.NoError(t, err)
		assert.Nil(t, spec.Schedule)
	})

	t.Run("the expression parses into occurrence times", func(t *testing.T) {
		schedule := manifest.Schedule{Cron: "0 2 * * *"}

		parsed, err := schedule.Parsed()
		require.NoError(t, err)

		// A daily expression names one time a day, so the occurrence after one is the
		// same time the next day.
		from := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
		assert.Equal(t, time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC), parsed.Next(from))
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
	require.NotNil(t, spec.Schedule)
	assert.Equal(t, "*/5 * * * *", spec.Schedule.Cron)
	assert.Equal(t, manifest.OverlapSkip, spec.Schedule.Overlap)
	require.NotNil(t, spec.Restart)
	assert.Equal(t, manifest.RestartOnFailure, spec.Restart.Policy)
	assert.Equal(t, 5, spec.Restart.Attempts)
	assert.Equal(t, 10*time.Second, spec.Restart.Delay)
	assert.NotEmpty(t, spec.Labels)
	require.NotNil(t, spec.Resources)
	assert.Equal(t, "512m", spec.Resources.Memory)
	assert.Equal(t, 0.5, spec.Resources.CPU)
	assert.Equal(t, 100, spec.Resources.Pids)
	require.NotNil(t, spec.Container)
	assert.NotEmpty(t, spec.Container.Image)
	assert.Equal(t, manifest.PullAlways, spec.Container.Pull)
	assert.NotEmpty(t, spec.Container.Command)
	assert.NotEmpty(t, spec.Container.User)
	assert.True(t, spec.Container.ReadOnly)
	assert.NotEmpty(t, spec.Container.CapAdd)
	assert.NotEmpty(t, spec.Container.CapDrop)
	assert.NotEmpty(t, spec.Env)
	assert.NotEmpty(t, spec.Ports)
	assert.Equal(t, "http", spec.Ports[0].Name)
	assert.NotEmpty(t, spec.Volumes)
}
