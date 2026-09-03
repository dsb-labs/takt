package wire_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/wire"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestToService(t *testing.T) {
	t.Parallel()

	t.Run("maps every field", func(t *testing.T) {
		t.Parallel()

		labels := api.Labels{"team": "platform"}
		protocol := api.ServiceTargetProtocol("udp")

		got := wire.ToService(api.ServiceSpec{
			Version: "v1",
			Name:    "example",
			Labels:  &labels,
			Target: api.ServiceTarget{
				Labels:   api.Labels{"app": "web"},
				Port:     53,
				Protocol: &protocol,
			},
		})

		assert.Equal(t, manifest.Service{
			Version: "v1",
			Name:    "example",
			Labels:  map[string]string{"team": "platform"},
			Target: manifest.ServiceTarget{
				Labels:   map[string]string{"app": "web"},
				Port:     53,
				Protocol: manifest.ProtocolUDP,
			},
		}, got)
	})

	t.Run("resolves the protocol default", func(t *testing.T) {
		t.Parallel()

		// A service reaching orca over HTTP means what the same service written
		// as a manifest file means, so the unset protocol resolves here too.
		got := wire.ToService(api.ServiceSpec{
			Version: "v1",
			Name:    "example",
			Target: api.ServiceTarget{
				Labels: api.Labels{"app": "web"},
				Port:   8080,
			},
		})

		assert.Equal(t, manifest.ProtocolTCP, got.Target.Protocol)
	})
}

func TestFromService(t *testing.T) {
	t.Parallel()

	spec := wire.FromService(manifest.Service{
		Version: "v1",
		Name:    "example",
		Labels:  map[string]string{"team": "platform"},
		Target: manifest.ServiceTarget{
			Labels:   map[string]string{"app": "web"},
			Port:     8080,
			Protocol: manifest.ProtocolTCP,
		},
	})

	assert.Equal(t, "v1", spec.Version)
	assert.Equal(t, "example", spec.Name)
	assert.NotNil(t, spec.Labels)
	assert.Equal(t, api.Labels{"app": "web"}, spec.Target.Labels)
	assert.Equal(t, 8080, spec.Target.Port)
	assert.NotNil(t, spec.Target.Protocol)
	assert.Equal(t, api.ServiceTargetProtocol("tcp"), *spec.Target.Protocol)

	// The round trip holds, so what a client submits is what the server decodes.
	assert.Equal(t, manifest.ProtocolTCP, wire.ToService(spec).Target.Protocol)
}
