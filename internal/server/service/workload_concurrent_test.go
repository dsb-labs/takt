package service_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver/docker"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/service"
)

// TestWorkloadService_Apply_Concurrent covers what the retry loop in store exists for,
// against a real database rather than a mocked one.
//
// Allocation reads the ports already promised and then claims one, so two applies
// racing each other can pick the same free port and one of them loses the unique
// constraint on the claim. Nothing about that is visible to a mock that answers
// whatever it was told to: the collision is the database's to report, so a test that
// does not have one cannot produce it.
//
// This is the shape an operator hits by applying a directory of manifests at once,
// which is what the CLI does.
func TestWorkloadService_Apply_Concurrent(t *testing.T) {
	t.Parallel()

	t.Run("settles every workload on a host port of its own", func(t *testing.T) {
		const workloads = 24

		svc, _ := newConcurrentTestService(t)

		// Every apply asks for a dynamic port, so every one of them races the others
		// through allocate-then-claim. The range is deliberately tight: a wide one
		// would let the allocator hand out a distinct port to each without them ever
		// having to contend, which is the case this test is not about.
		applied := make([]service.Workload, workloads)
		errs := make([]error, workloads)

		var wg sync.WaitGroup
		for i := range workloads {
			wg.Add(1)

			go func() {
				defer wg.Done()

				spec := containerSpec(workloadName(i), "example/example:latest")
				spec.Ports = &[]api.PortMapping{{To: 8080}}

				applied[i], _, errs[i] = svc.Apply(t.Context(), spec)
			}()
		}

		wg.Wait()

		hosts := make(map[int]string, workloads)
		for i := range workloads {
			require.NoErrorf(t, errs[i], "applying %s", workloadName(i))
			require.Len(t, applied[i].Ports, 1)

			host := applied[i].Ports[0].From

			// Two workloads reported the same host port, which is the failure the
			// claim's unique constraint and the retry above it exist to prevent.
			previous, clash := hosts[host]
			require.Falsef(t, clash, "%s and %s both hold host port %d", previous, applied[i].Name, host)

			hosts[host] = applied[i].Name
		}
	})

	t.Run("tells a caller their pinned port is taken rather than moving it", func(t *testing.T) {
		svc, _ := newConcurrentTestService(t)

		first := containerSpec("first", "example/example:latest")
		first.Ports = &[]api.PortMapping{{From: ptr(21000), To: 8080}}

		_, _, err := svc.Apply(t.Context(), first)
		require.NoError(t, err)

		// A dynamic port that collides is orca's to resolve, so it is allocated again.
		// A pinned one is the caller's decision, and quietly moving it would hand back
		// a workload reachable somewhere other than where they asked for.
		second := containerSpec("second", "example/example:latest")
		second.Ports = &[]api.PortMapping{{From: ptr(21000), To: 8080}}

		_, _, err = svc.Apply(t.Context(), second)
		assert.ErrorIs(t, err, service.ErrHostPortTaken)
	})

	t.Run("gives up rather than retrying forever", func(t *testing.T) {
		svc, ports := newConcurrentTestService(t)

		// Pinned, so that the port the allocator below hands out is one something
		// definitely holds. The real allocator picks at random across its range, which
		// would make whether these two collide a matter of luck.
		taken := containerSpec("taken", "example/example:latest")
		taken.Ports = &[]api.PortMapping{{From: ptr(20000), To: 8080}}

		_, _, err := svc.Apply(t.Context(), taken)
		require.NoError(t, err)

		// Allocation avoids what the repository reports as allocated, so the collision
		// only happens if the allocator is told nothing is. The claim still fails,
		// because the database holds what the reader does not report. A stub allocator
		// rather than the real one, which checks a candidate is bindable on this host
		// and would otherwise report the range exhausted instead.
		blind := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:    newTestLogger(t),
			Drivers:   map[string]service.Driver{docker.Name: newMockDriver(t)},
			Workloads: database.NewWorkloadRepository(ports),
			Ports:     blindPorts{PortRepository: database.NewPortRepository(ports)},
			Allocator: allocatorStub{},
		})

		contender := containerSpec("contender", "example/example:latest")
		contender.Ports = &[]api.PortMapping{{To: 8080}}

		_, _, err = blind.Apply(t.Context(), contender)
		require.ErrorIs(t, err, service.ErrHostPortTaken)
		// A pinned collision reports the same sentinel, so the wording is what says
		// this was the bound being reached rather than a port the caller asked for.
		assert.Contains(t, err.Error(), "gave up after")
	})
}

type (
	// The blindPorts type reports that nothing is allocated, whatever is.
	//
	// Allocation avoids the ports the repository names, so the only way to make it
	// choose one that is already claimed is to stop it hearing about the claim. What
	// the database enforces is untouched, which is the part under test.
	blindPorts struct {
		service.PortRepository
	}
)

func (b blindPorts) Allocated(context.Context) ([]int, error) {
	return nil, nil
}

// newConcurrentTestService builds a service over a real database, since the collision
// these tests are about is one only the claim's unique constraint produces.
func newConcurrentTestService(t *testing.T) (*service.WorkloadService, *sql.DB) {
	t.Helper()

	db, err := database.Open(t.Context(), database.Config{
		Logger: newTestLogger(t),
		Path:   filepath.Join(t.TempDir(), "test.db"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	driver := newMockDriver(t)
	driver.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

	svc := service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: driver},
		Workloads: database.NewWorkloadRepository(db),
		Ports:     database.NewPortRepository(db),
		// Narrow enough that concurrent applies contend for the same ports rather
		// than each being handed one nothing else wanted.
		Allocator: port.New(port.Config{Min: 21000, Max: 21031}),
	})

	return svc, db
}

func workloadName(i int) string {
	return "workload-" + strconv.Itoa(i)
}

func ptr[T any](v T) *T {
	return &v
}
