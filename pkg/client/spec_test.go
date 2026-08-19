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
		require.NotNil(t, wire.Container)
		assert.Nil(t, wire.Container.Env)
		assert.Nil(t, wire.Container.Ports)
		assert.Nil(t, wire.Script)
	})

	t.Run("round-trips a full specification", func(t *testing.T) {
		spec := manifest.Spec{
			Version:  "v1",
			Name:     "example",
			Schedule: "*/5 * * * *",
			Labels:   map[string]string{"some-key": "some-value"},
			Container: &manifest.Container{
				Image: "example/example:latest",
				Env:   map[string]string{"EXAMPLE": "EXAMPLE"},
				Ports: []manifest.Port{{To: 8080, From: 4141}},
			},
		}

		// A specification that survives a round trip unchanged is what lets the
		// client submit what it parsed and read back something comparable.
		assert.Equal(t, spec, newSpec(wireSpec(spec)))
	})

	t.Run("round-trips a script specification", func(t *testing.T) {
		spec := manifest.Spec{
			Version: "v1",
			Name:    "example",
			Script:  &manifest.Script{Raw: `echo "hello world"`},
		}

		assert.Equal(t, spec, newSpec(wireSpec(spec)))
	})
}
