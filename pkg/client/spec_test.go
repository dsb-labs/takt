package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestWireSpec(t *testing.T) {
	t.Parallel()

	t.Run("omits empty optional values", func(t *testing.T) {
		wire := wireSpec(manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
		})

		// Sending an empty schedule or an empty label map rather than omitting it
		// would change the specification the server hashes, making an unchanged
		// manifest look like an update.
		assert.Nil(t, wire.Schedule)
		assert.Nil(t, wire.Labels)
		assert.Nil(t, wire.Restart)
		assert.Nil(t, wire.Resources)
		require.NotNil(t, wire.Container)
		assert.Nil(t, wire.Container.Pull)
		assert.Nil(t, wire.Container.Command)
		assert.Nil(t, wire.Container.User)
		assert.Nil(t, wire.Container.ReadOnly)
		assert.Nil(t, wire.Container.CapAdd)
		assert.Nil(t, wire.Container.CapDrop)
		assert.Nil(t, wire.Env)
		assert.Nil(t, wire.Ports)
		assert.Nil(t, wire.Exec)
	})

	t.Run("omits the default pull policy when named", func(t *testing.T) {
		wire := wireSpec(manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest", Pull: manifest.PullMissing},
		})

		// A manifest that names the default has to encode as one that says
		// nothing, or writing "pull: missing" into an existing manifest would
		// move its hash and replace its instance for no change in behaviour.
		require.NotNil(t, wire.Container)
		assert.Nil(t, wire.Container.Pull)
	})

	t.Run("round-trips a full specification", func(t *testing.T) {
		spec := manifest.Spec{
			Version:  "v1",
			Name:     "example",
			Schedule: &manifest.Schedule{Cron: "*/5 * * * *", Overlap: manifest.OverlapReplace},
			Labels:   map[string]string{"some-key": "some-value"},
			// Resolved rather than empty, because a round trip runs the value
			// through NewSpec, which applies the defaults.
			Restart:   &manifest.Restart{Policy: manifest.RestartAlways, Delay: manifest.DefaultRestartDelay},
			Ports:     []manifest.Port{{To: 8080, From: 4141, Protocol: manifest.ProtocolTCP}},
			Env:       map[string]string{"EXAMPLE": "EXAMPLE"},
			Resources: &manifest.Resources{Memory: "512m", CPU: 0.5, Pids: 100},
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
		assert.Equal(t, spec, manifest.NewSpec(wireSpec(spec)))
	})

	t.Run("round-trips an exec specification", func(t *testing.T) {
		spec := manifest.Spec{
			Version: "v1",
			Name:    "example",
			Restart: &manifest.Restart{Policy: manifest.RestartAlways, Delay: manifest.DefaultRestartDelay},
			Exec:    &manifest.Exec{Command: []string{"echo", "hello world"}},
		}

		assert.Equal(t, spec, manifest.NewSpec(wireSpec(spec)))
	})
}
