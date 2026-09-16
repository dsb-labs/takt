package service_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestNodeService_Get(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name           string
		VolumesCreated bool
	}{
		{
			// The volumes directory is made by the first volume, so a fresh
			// server has none, and reading the node must not create it.
			Name: "reports the volumes directory before it exists",
		},
		{
			Name:           "reports the volumes directory once it exists",
			VolumesCreated: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			dataDirectory := t.TempDir()
			volumesDirectory := filepath.Join(dataDirectory, "volumes")
			if tc.VolumesCreated {
				require.NoError(t, os.Mkdir(volumesDirectory, 0o700))
			}

			workloads := NewMockWorkloadLister(t)
			workloads.EXPECT().List(mock.Anything).Return(testFleet(), nil).Once()

			startedAt := time.Now().Add(-time.Minute)
			svc := service.NewNodeService(service.NodeServiceConfig{
				Workloads:        workloads,
				DataDirectory:    dataDirectory,
				VolumesDirectory: volumesDirectory,
				Version:          "v1.2.3",
				StartedAt:        startedAt,
			})

			node, err := svc.Get(t.Context())
			require.NoError(t, err)

			hostname, err := os.Hostname()
			require.NoError(t, err)

			assert.Equal(t, hostname, node.Hostname)
			assert.Equal(t, runtime.GOOS, node.OS)
			assert.Equal(t, runtime.GOARCH, node.Arch)
			assert.Equal(t, runtime.NumCPU(), node.CPUs)
			assert.NotEmpty(t, node.Kernel)
			assert.Equal(t, "v1.2.3", node.Version)
			assert.Equal(t, startedAt, node.StartedAt)

			// The host booted before this process started, and the instant is
			// read from the kernel rather than derived from the clock.
			assert.True(t, node.BootedAt.Before(startedAt))

			assert.Positive(t, node.Memory.Total)
			assert.Positive(t, node.Memory.Used)
			assert.LessOrEqual(t, node.Memory.Used, node.Memory.Total)

			assert.GreaterOrEqual(t, node.Load.One, 0.0)
			assert.GreaterOrEqual(t, node.Load.Five, 0.0)
			assert.GreaterOrEqual(t, node.Load.Fifteen, 0.0)

			assert.Equal(t, dataDirectory, node.Disks.Data.Path)
			assert.Positive(t, node.Disks.Data.Total)
			assert.LessOrEqual(t, node.Disks.Data.Free, node.Disks.Data.Total)

			// Reported under the configured path either way, and the two are
			// one filesystem here, so the figures agree.
			assert.Equal(t, volumesDirectory, node.Disks.Volumes.Path)
			assert.Equal(t, node.Disks.Data.Total, node.Disks.Volumes.Total)

			// Two running instances at 512m and half a processor, one running
			// with no limits, and one exited that promised nothing. The memory
			// is the figure the runtimes enforce, so the manifest's size is
			// summed as bytes.
			assert.Equal(t, 2*512<<20, node.Allocated.Memory)
			assert.InDelta(t, 1.0, node.Allocated.CPU, 0)
			assert.Equal(t, 1, node.Allocated.UnlimitedMemory)
			assert.Equal(t, 1, node.Allocated.UnlimitedCPU)

			if !tc.VolumesCreated {
				_, err := os.Stat(volumesDirectory)
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func testFleet() []service.Workload {
	return []service.Workload{
		{
			Name: "limited",
			Spec: manifest.Spec{Resources: &manifest.Resources{Memory: "512m", CPU: 0.5}},
			Instances: []service.Instance{
				{Instance: driver.Instance{ID: "a", State: driver.StateRunning}},
				{Instance: driver.Instance{ID: "b", State: driver.StateRunning}},
				{Instance: driver.Instance{ID: "c", State: driver.StateExited}},
			},
		},
		{
			Name: "unlimited",
			Instances: []service.Instance{
				{Instance: driver.Instance{ID: "d", State: driver.StateRunning}},
			},
		},
	}
}
