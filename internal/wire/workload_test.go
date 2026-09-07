package wire_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestToSpec(t *testing.T) {
	t.Parallel()

	// A manifest reaches the server two ways, decoded from YAML and converted from
	// the wire. A default applied to only one of them would make the same manifest
	// behave differently depending on the route it took.
	t.Run("resolves the defaults a manifest file would get", func(t *testing.T) {
		spec, err := wire.ToSpec(api.WorkloadSpec{
			Version:   "v1",
			Name:      "example",
			Container: &api.ContainerSpec{Image: "example/example:latest"},
		})
		require.NoError(t, err)

		require.NotNil(t, spec.Restart)
		assert.Equal(t, manifest.RestartAlways, spec.Restart.Policy)
		assert.Equal(t, manifest.DefaultRestartDelay, spec.Restart.Delay)
	})

	t.Run("rejects a restart delay that is not a duration", func(t *testing.T) {
		_, err := wire.ToSpec(api.WorkloadSpec{
			Version:   "v1",
			Name:      "example",
			Restart:   &api.RestartSpec{Delay: new("half an hour")},
			Container: &api.ContainerSpec{Image: "example/example:latest"},
		})

		assert.ErrorContains(t, err, `"half an hour" is not a duration`)
	})

	t.Run("rejects a health check timing that is not a duration", func(t *testing.T) {
		_, err := wire.ToSpec(api.WorkloadSpec{
			Version:   "v1",
			Name:      "example",
			Health:    &api.HealthSpec{HTTP: new("/healthz"), Interval: new("often")},
			Container: &api.ContainerSpec{Image: "example/example:latest"},
		})

		assert.ErrorContains(t, err, `"often" is not a duration`)
	})
}

func TestFromSpec(t *testing.T) {
	t.Parallel()

	t.Run("omits empty optional values", func(t *testing.T) {
		spec := wire.FromSpec(manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
		})

		// Sending an empty schedule or an empty label map rather than omitting it
		// would change the specification the server hashes, making an unchanged
		// manifest look like an update.
		assert.Nil(t, spec.Schedule)
		assert.Nil(t, spec.Labels)
		assert.Nil(t, spec.Restart)
		assert.Nil(t, spec.Resources)
		require.NotNil(t, spec.Container)
		assert.Nil(t, spec.Container.Pull)
		assert.Nil(t, spec.Container.Command)
		assert.Nil(t, spec.Container.User)
		assert.Nil(t, spec.Container.ReadOnly)
		assert.Nil(t, spec.Container.CapAdd)
		assert.Nil(t, spec.Container.CapDrop)
		assert.Nil(t, spec.Env)
		assert.Nil(t, spec.Ports)
		assert.Nil(t, spec.Exec)
	})

	t.Run("omits the default pull policy when named", func(t *testing.T) {
		spec := wire.FromSpec(manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest", Pull: manifest.PullMissing},
		})

		// A manifest that names the default has to encode as one that says
		// nothing, or writing "pull: missing" into an existing manifest would
		// move its hash and replace its instance for no change in behaviour.
		require.NotNil(t, spec.Container)
		assert.Nil(t, spec.Container.Pull)
	})

	t.Run("round-trips a full specification", func(t *testing.T) {
		spec := manifest.Spec{
			Version: "v1",
			Name:    "example",
			// Resolved rather than zero for the reason the restart policy is.
			Count:    1,
			Schedule: &manifest.Schedule{Cron: "*/5 * * * *", Overlap: manifest.OverlapReplace},
			Labels:   map[string]string{"some-key": "some-value"},
			// Resolved rather than empty, because a round trip runs the value
			// through ToSpec, which applies the defaults.
			Restart:   &manifest.Restart{Policy: manifest.RestartAlways, Delay: manifest.DefaultRestartDelay},
			Ports:     []manifest.Port{{Name: "http", To: 8080, From: 4141, Protocol: manifest.ProtocolTCP}},
			Env:       map[string]string{"EXAMPLE": "EXAMPLE"},
			Resources: &manifest.Resources{Memory: "512m", CPU: 0.5, Pids: 100},
			Volumes: []manifest.VolumeMount{
				{Name: "example-data", To: "/var/lib/example", ReadOnly: true},
				{Path: "/mnt/media", To: "/media", ReadOnly: true, Propagation: "rslave"},
				{Secret: "tls-cert", To: "/etc/tls/cert.pem", Signal: manifest.SignalHUP},
			},
			Container: &manifest.Container{
				Image:    "example/example:latest",
				Pull:     manifest.PullAlways,
				Command:  []string{"sh", "-c", "exit 0"},
				User:     "65532:65532",
				ReadOnly: true,
				CapAdd:   []string{"NET_ADMIN"},
				CapDrop:  []string{"ALL"},
			},
		}

		// A specification that survives a round trip unchanged is what lets the
		// client submit what it parsed and read back something comparable.
		actual, err := wire.ToSpec(wire.FromSpec(spec))
		require.NoError(t, err)
		assert.Equal(t, spec, actual)
	})

	t.Run("round-trips an exec specification", func(t *testing.T) {
		spec := manifest.Spec{
			Version: "v1",
			Name:    "example",
			Count:   1,
			Restart: &manifest.Restart{Policy: manifest.RestartAlways, Delay: manifest.DefaultRestartDelay},
			Exec:    &manifest.Exec{Command: []string{"echo", "hello world"}},
		}

		actual, err := wire.ToSpec(wire.FromSpec(spec))
		require.NoError(t, err)
		assert.Equal(t, spec, actual)
	})
}
