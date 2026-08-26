package manifest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/pkg/manifest"
)

// TestSpec_JSON pins the JSON encoding of a specification, which is what orca stores
// a workload as and what its hash covers. A change here replaces every running
// instance on every node, so a failure means the encoding moved rather than that the
// fixture is stale.
func TestSpec_JSON(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name string
		File string
		Spec manifest.Spec
	}{
		{
			Name: "every field set",
			File: "spec_full.json",
			Spec: manifest.Spec{
				Version:  "v1",
				Name:     "api",
				Schedule: &manifest.Schedule{Cron: "*/5 * * * *", Overlap: manifest.OverlapSkip},
				Labels:   map[string]string{"team": "platform"},
				Ports: []manifest.Port{
					{Name: "http", To: 8080, From: 20000, Protocol: manifest.ProtocolTCP},
				},
				Env: map[string]string{"LOG_LEVEL": "debug"},
				Volumes: []manifest.VolumeMount{
					{Name: "data", To: "/var/lib/api"},
					{Secret: "api-key", To: "/etc/api/key", Signal: manifest.SignalHUP},
				},
				Restart: &manifest.Restart{
					Policy:   manifest.RestartOnFailure,
					Attempts: 3,
					Delay:    5 * time.Second,
				},
				Health: &manifest.Health{
					HTTP:        "/healthz",
					Port:        "http",
					Interval:    10 * time.Second,
					Timeout:     2 * time.Second,
					Retries:     3,
					StartPeriod: 30 * time.Second,
				},
				Resources: &manifest.Resources{Memory: "512m", CPU: 0.5, Pids: 128},
				Container: &manifest.Container{
					Image:    "ghcr.io/dsb-labs/api:v1",
					Pull:     manifest.PullAlways,
					Command:  []string{"/api", "serve"},
					User:     "1000:1000",
					ReadOnly: true,
					CapAdd:   []string{"NET_BIND_SERVICE"},
					CapDrop:  []string{"ALL"},
				},
			},
		},
		{
			// A specification setting nothing optional encodes as the two fields that
			// identify it and its runtime. That is what keeps a field added later from
			// re-hashing every workload that does not set it.
			Name: "nothing optional set",
			File: "spec_minimal.json",
			Spec: manifest.Spec{
				Version:   "v1",
				Name:      "worker",
				Container: &manifest.Container{Image: "ghcr.io/dsb-labs/worker:v1"},
			},
		},
		{
			Name: "exec runtime",
			File: "spec_exec.json",
			Spec: manifest.Spec{
				Version: "v1",
				Name:    "backup",
				Exec:    &manifest.Exec{Command: []string{"/usr/local/bin/backup"}},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			expected, err := os.ReadFile(filepath.Join("testdata", tc.File))
			require.NoError(t, err)

			actual, err := json.Marshal(tc.Spec)
			require.NoError(t, err)

			assert.JSONEq(t, string(expected), string(actual))

			// The bytes themselves are pinned, not just the object they describe. Two
			// encodings that differ only in field order hash differently, so JSONEq
			// alone would let the hash move without failing.
			assert.Equal(t, string(expected), string(actual))
		})
	}
}
