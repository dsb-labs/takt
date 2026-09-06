package driver_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestNewWorkload(t *testing.T) {
	t.Parallel()

	t.Run("maps resolved volume mounts", func(t *testing.T) {
		w, err := driver.NewWorkload(row(t, manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
			Volumes: []manifest.VolumeMount{
				{Name: "example-data", From: "/var/lib/takt/volumes/abc", To: "/var/lib/example", ReadOnly: true},
				// Unresolved, so the server has not finished settling the
				// workload. Mounting nothing would be worse than waiting.
				{Name: "pending-data", To: "/var/lib/pending"},
				// A mounted value is written as the workload starts and added by
				// whoever wrote it, so it is not resolved here.
				{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
			},
		}))
		require.NoError(t, err)

		assert.Equal(t, []driver.Volume{
			{Name: "example-data", Host: "/var/lib/takt/volumes/abc", Target: "/var/lib/example", ReadOnly: true},
		}, w.Volumes)
	})

	t.Run("maps path mounts without resolution", func(t *testing.T) {
		w, err := driver.NewWorkload(row(t, manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
			Volumes: []manifest.VolumeMount{
				{Path: "/mnt/media", To: "/media", ReadOnly: true},
				{Path: "/var/run/docker.sock", To: "/var/run/docker.sock"},
			},
		}))
		require.NoError(t, err)

		// The path is both where the data is and what the mount is called, since a
		// path mount has no name of its own.
		assert.Equal(t, []driver.Volume{
			{Name: "/mnt/media", Host: "/mnt/media", Target: "/media", ReadOnly: true},
			{Name: "/var/run/docker.sock", Host: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
		}, w.Volumes)
	})
}

// row encodes a specification the way the server stores one, so the mapping under
// test reads what a driver would actually be given.
func row(t *testing.T, spec manifest.Spec) database.Workload {
	t.Helper()

	encoded, err := json.Marshal(spec)
	require.NoError(t, err)

	return database.Workload{ID: "cvhs0dq0kqj4c9r8m1a0", Name: spec.Name, Version: 1, Spec: encoded}
}
