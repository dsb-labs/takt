package client_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
)

func TestClient_GetNode(t *testing.T) {
	t.Parallel()

	node := api.Node{
		Hostname:  "node-1",
		Os:        "linux",
		Arch:      "amd64",
		Kernel:    "6.12.0-1-amd64",
		Cpus:      8,
		Version:   "v0.4.0",
		StartedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		BootedAt:  time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
		Memory:    api.NodeMemory{Total: 32 << 30, Used: 12 << 30},
		Load:      api.NodeLoad{One: 0.5, Five: 0.75, Fifteen: 1.25},
		Disks: api.NodeDisks{
			Data:    api.NodeDisk{Path: "/var/lib/takt", Total: 500 << 30, Free: 320 << 30},
			Volumes: api.NodeDisk{Path: "/var/lib/takt/volumes", Total: 500 << 30, Free: 320 << 30},
		},
	}

	tt := []struct {
		Name         string
		Handler      http.HandlerFunc
		ExpectsError bool
	}{
		{
			Name: "returns the node",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/node", r.URL.Path)

				writeJSON(t, w, http.StatusOK, api.GetNodeResult{Node: node})
			},
		},
		{
			Name: "server failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError, api.ErrorResponse{Error: "broken"})
			},
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := newTestClient(t, tc.Handler)

			result, err := c.GetNode(t.Context())
			if tc.ExpectsError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, node.Hostname, result.Hostname)
			assert.Equal(t, node.Os, result.OS)
			assert.Equal(t, node.Arch, result.Arch)
			assert.Equal(t, node.Kernel, result.Kernel)
			assert.Equal(t, node.Cpus, result.CPUs)
			assert.Equal(t, node.Version, result.Version)
			assert.True(t, node.StartedAt.Equal(result.StartedAt))
			assert.True(t, node.BootedAt.Equal(result.BootedAt))
			assert.Equal(t, node.Memory.Total, result.Memory.Total)
			assert.Equal(t, node.Memory.Used, result.Memory.Used)
			assert.InDelta(t, node.Load.Fifteen, result.Load.Fifteen, 0)
			assert.Equal(t, node.Disks.Data.Path, result.Disks.Data.Path)
			assert.Equal(t, node.Disks.Volumes.Free, result.Disks.Volumes.Free)
		})
	}
}
