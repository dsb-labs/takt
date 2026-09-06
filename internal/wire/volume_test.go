package wire_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/internal/wire"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestFromVolume(t *testing.T) {
	t.Parallel()

	t.Run("omits empty optional values", func(t *testing.T) {
		spec := wire.FromVolume(manifest.Volume{Version: "v1", Name: "example-data"})

		assert.Nil(t, spec.Labels)
		assert.Nil(t, spec.Owner)
		assert.Nil(t, spec.Mode)
	})

	t.Run("round-trips a full volume", func(t *testing.T) {
		volume := manifest.Volume{
			Version: "v1",
			Name:    "example-data",
			Labels:  map[string]string{"app": "web"},
			Owner:   "470:470",
			Mode:    "0755",
		}

		assert.Equal(t, volume, wire.ToVolume(wire.FromVolume(volume)))
	})
}
