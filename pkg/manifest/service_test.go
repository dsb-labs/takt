package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestParseService(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Expected     manifest.Service
		ExpectsError bool
	}{
		{
			Name: "a full service manifest",
			File: "service.yaml",
			Expected: manifest.Service{
				Version: "v1",
				Name:    "example",
				Labels:  map[string]string{"app": "web"},
				Target: manifest.ServiceTarget{
					Labels:   map[string]string{"app": "web", "tier": "backend"},
					Port:     8080,
					Protocol: manifest.ProtocolTCP,
				},
			},
		},
		{
			// A manifest naming no protocol asks for TCP, resolved here so nothing
			// downstream has to decide what an unset protocol means.
			Name: "a minimal service manifest takes the protocol default",
			File: "service_minimal.yaml",
			Expected: manifest.Service{
				Version: "v1",
				Name:    "example",
				Target: manifest.ServiceTarget{
					Labels:   map[string]string{"app": "web"},
					Port:     8080,
					Protocol: manifest.ProtocolTCP,
				},
			},
		},
		{
			Name: "a udp service manifest",
			File: "service_udp.yaml",
			Expected: manifest.Service{
				Version: "v1",
				Name:    "example",
				Target: manifest.ServiceTarget{
					Labels:   map[string]string{"app": "dns"},
					Port:     53,
					Protocol: manifest.ProtocolUDP,
				},
			},
		},
		{
			// A service selecting nothing would select everything, which is more
			// likely a mistake than an intent.
			Name:         "rejects a manifest naming no target",
			File:         "service_no_target.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a target naming no labels",
			File:         "service_no_target_labels.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a port outside the usable range",
			File:         "service_bad_port.yaml",
			ExpectsError: true,
		},
		{
			// The selected workloads need not agree on their port names, so a
			// target names the port itself rather than a port name.
			Name:         "rejects a port written as a name",
			File:         "service_named_port.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a protocol orca cannot publish",
			File:         "service_bad_protocol.yaml",
			ExpectsError: true,
		},
		{
			// The service API serves its own routes under the path a service of
			// that name would occupy.
			Name:         "rejects the reserved name",
			File:         "service_reserved_name.yaml",
			ExpectsError: true,
		},
		{
			// A service's labels answer to the same rules a workload's do,
			// reserved prefix included, so an operator learns them once.
			Name:         "rejects a label orca reserves for itself",
			File:         "service_bad_label.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects a target label orca reserves for itself",
			File:         "service_bad_target_label.yaml",
			ExpectsError: true,
		},
		{
			Name:         "rejects an unknown field",
			File:         "service_unknown_field.yaml",
			ExpectsError: true,
		},
		{
			// Which resource a file describes is decided by what it is given to,
			// so a workload manifest handed to this reads as unknown keys.
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

			service, err := manifest.ParseService(f)
			if tc.ExpectsError {
				assert.Error(t, err)
				assert.Zero(t, service)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, service)
		})
	}
}
