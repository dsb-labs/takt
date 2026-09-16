package api_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	generated "github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
)

func TestNodeAPI_GetNode(t *testing.T) {
	t.Parallel()

	t.Run("reports the node", func(t *testing.T) {
		node := testNode()

		svc := NewMockNodeService(t)
		svc.EXPECT().Get().Return(node, nil).Once()

		resp := doNode(t, svc)
		require.Equal(t, http.StatusOK, resp.Code)

		var result generated.GetNodeResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))

		assert.Equal(t, node.Hostname, result.Node.Hostname)
		assert.Equal(t, node.OS, result.Node.Os)
		assert.Equal(t, node.Arch, result.Node.Arch)
		assert.Equal(t, node.Kernel, result.Node.Kernel)
		assert.Equal(t, node.CPUs, result.Node.Cpus)
		assert.Equal(t, node.Version, result.Node.Version)
		assert.True(t, node.StartedAt.Equal(result.Node.StartedAt))
		assert.True(t, node.BootedAt.Equal(result.Node.BootedAt))
		assert.Equal(t, node.Memory.Total, result.Node.Memory.Total)
		assert.Equal(t, node.Memory.Used, result.Node.Memory.Used)
		assert.InDelta(t, node.Load.One, result.Node.Load.One, 0)
		assert.InDelta(t, node.Load.Five, result.Node.Load.Five, 0)
		assert.InDelta(t, node.Load.Fifteen, result.Node.Load.Fifteen, 0)
		assert.Equal(t, node.Disks.Data.Path, result.Node.Disks.Data.Path)
		assert.Equal(t, node.Disks.Data.Free, result.Node.Disks.Data.Free)
		assert.Equal(t, node.Disks.Volumes.Path, result.Node.Disks.Volumes.Path)
		assert.Equal(t, node.Disks.Volumes.Total, result.Node.Disks.Volumes.Total)
	})

	t.Run("answers 500 when the node cannot be read", func(t *testing.T) {
		svc := NewMockNodeService(t)
		svc.EXPECT().Get().Return(service.Node{}, errors.New("meminfo is gone")).Once()

		resp := doNode(t, svc)
		require.Equal(t, http.StatusInternalServerError, resp.Code)
		assert.JSONEq(t, `{"error":"failed to get node"}`, resp.Body.String())
	})
}

func doNode(t *testing.T, node api.NodeService) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since
	// the generated router serves one API and a request for an unregistered
	// route would come back as a routing failure rather than as the handler's
	// answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: NewMockVolumeService(t)}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: NewMockSecretService(t)}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: NewMockVariableService(t)}),
		Node:      api.NewNodeAPI(api.NodeAPIConfig{Logger: logger, Node: node}),
	}).Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/node", nil)
	resp := httptest.NewRecorder()
	middleware.Authenticate(nil)(mux).ServeHTTP(resp, req)

	return resp
}

func testNode() service.Node {
	return service.Node{
		Hostname:  "node-1",
		OS:        "linux",
		Arch:      "amd64",
		Kernel:    "6.12.0-1-amd64",
		CPUs:      8,
		Version:   "v0.4.0",
		StartedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		BootedAt:  time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
		Memory:    service.NodeMemory{Total: 32 << 30, Used: 12 << 30},
		Load:      service.NodeLoad{One: 0.5, Five: 0.75, Fifteen: 1.25},
		Disks: service.NodeDisks{
			Data:    service.NodeDisk{Path: "/var/lib/takt", Total: 500 << 30, Free: 320 << 30},
			Volumes: service.NodeDisk{Path: "/var/lib/takt/volumes", Total: 500 << 30, Free: 320 << 30},
		},
	}
}
