package service_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// newServiceService constructs a ServiceService over the given mocks, reporting
// backends at the test host address.
func newServiceService(t *testing.T, repo *MockServiceRepository, workloads *MockWorkloadLister) *service.ServiceService {
	t.Helper()

	return service.NewServiceService(service.ServiceServiceConfig{
		Logger:    slog.New(slog.DiscardHandler),
		Services:  repo,
		Workloads: workloads,
		Address:   "203.0.113.10",
	})
}

// storedService returns a service row as the repository would report it,
// targeting workloads labelled app=web on 8080/tcp.
func storedService() database.Service {
	return database.Service{
		ID:             "svc-1",
		Name:           "example",
		TargetLabels:   map[string]string{"app": "web"},
		TargetPort:     8080,
		TargetProtocol: "tcp",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
}

// targetedWorkload returns a hydrated workload publishing 8080/tcp for each of
// the given instances, each mapped to a host port of 20000 plus its index.
func targetedWorkload(name string, instances ...driver.Instance) service.Workload {
	workload := service.Workload{
		Name:      name,
		Instances: instances,
	}

	for _, instance := range instances {
		workload.Ports = append(workload.Ports, service.ResolvedPort{
			Instance: instance.Index,
			To:       8080,
			From:     20000 + instance.Index,
			Protocol: manifest.ProtocolTCP,
			Dynamic:  true,
		})
	}

	return workload
}

func TestServiceService_Apply(t *testing.T) {
	t.Parallel()

	t.Run("stores a valid service and resolves its backends", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().Upsert(mock.Anything, database.Service{
			Name:           "example",
			TargetLabels:   map[string]string{"app": "web"},
			TargetPort:     8080,
			TargetProtocol: "tcp",
		}).Return(storedService(), true, nil).Once()

		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything, []string{`$.labels."app"=web`}).
			Return([]service.Workload{
				targetedWorkload("web", driver.Instance{Index: 0, State: driver.StateRunning}),
			}, nil).Once()

		svc := newServiceService(t, repo, workloads)

		applied, created, err := svc.Apply(t.Context(), manifest.Service{
			Version: "v1",
			Name:    "example",
			Target: manifest.ServiceTarget{
				Labels: map[string]string{"app": "web"},
				Port:   8080,
			},
		})
		require.NoError(t, err)

		assert.True(t, created)
		assert.Equal(t, "example", applied.Name)

		// The protocol was left unset, so the default was resolved before the
		// row was written and is what the target reports.
		assert.Equal(t, manifest.ProtocolTCP, applied.Target.Protocol)
		assert.Equal(t, []service.Backend{
			{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
		}, applied.Backends)
	})

	t.Run("refuses an invalid service before anything is written", func(t *testing.T) {
		t.Parallel()

		// The repository expects no calls: validation failing must leave no row.
		repo := NewMockServiceRepository(t)
		workloads := NewMockWorkloadLister(t)

		svc := newServiceService(t, repo, workloads)

		_, _, err := svc.Apply(t.Context(), manifest.Service{
			Version: "v1",
			Name:    "example",
			Target:  manifest.ServiceTarget{Port: 8080, Protocol: manifest.ProtocolTCP},
		})
		assert.ErrorIs(t, err, service.ErrInvalidService)
	})
}

func TestServiceService_Get(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Selected []service.Workload
		Expected []service.Backend
	}{
		{
			Name: "reports every running instance across the selected workloads",
			Selected: []service.Workload{
				targetedWorkload("web-b", driver.Instance{Index: 0, State: driver.StateRunning}),
				targetedWorkload("web-a",
					driver.Instance{Index: 1, State: driver.StateRunning},
					driver.Instance{Index: 0, State: driver.StateRunning},
				),
			},
			Expected: []service.Backend{
				{Workload: "web-a", Instance: 0, Address: "203.0.113.10:20000"},
				{Workload: "web-a", Instance: 1, Address: "203.0.113.10:20001"},
				{Workload: "web-b", Instance: 0, Address: "203.0.113.10:20000"},
			},
		},
		{
			// Health is folded into the observed state before the workload
			// reaches this package, so anything not running — pending, failed
			// its check, exited — is one rule rather than several.
			Name: "reports only the instances observed running",
			Selected: []service.Workload{
				targetedWorkload("web",
					driver.Instance{Index: 0, State: driver.StateRunning},
					driver.Instance{Index: 1, State: driver.StatePending},
					driver.Instance{Index: 2, State: driver.StateFailed},
					driver.Instance{Index: 3, State: driver.StateTerminating},
				),
			},
			Expected: []service.Backend{
				{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
			},
		},
		{
			// A balancer told about a workload being torn down would keep
			// sending requests to addresses about to vanish.
			Name: "drains a workload being deleted",
			Selected: []service.Workload{
				func() service.Workload {
					workload := targetedWorkload("web", driver.Instance{Index: 0, State: driver.StateRunning})
					workload.Deleting = true

					return workload
				}(),
			},
			Expected: nil,
		},
		{
			// A label selector is allowed to span workloads where only some
			// publish the target port.
			Name: "skips a selected workload that does not publish the port",
			Selected: []service.Workload{
				{
					Name:      "worker",
					Instances: []driver.Instance{{Index: 0, State: driver.StateRunning}},
				},
			},
			Expected: nil,
		},
		{
			// TCP and UDP are separate address spaces, so a udp mapping on the
			// target port is not the port the service addresses.
			Name: "skips a port published on the wrong protocol",
			Selected: []service.Workload{
				{
					Name:      "dns",
					Instances: []driver.Instance{{Index: 0, State: driver.StateRunning}},
					Ports: []service.ResolvedPort{
						{Instance: 0, To: 8080, From: 20000, Protocol: manifest.ProtocolUDP},
					},
				},
			},
			Expected: nil,
		},
		{
			Name:     "reports no backends when nothing is selected",
			Selected: nil,
			Expected: nil,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			repo := NewMockServiceRepository(t)
			repo.EXPECT().Get(mock.Anything, "example").Return(storedService(), nil).Once()

			workloads := NewMockWorkloadLister(t)
			workloads.EXPECT().List(mock.Anything, []string{`$.labels."app"=web`}).
				Return(tc.Selected, nil).Once()

			svc := newServiceService(t, repo, workloads)

			got, err := svc.Get(t.Context(), "example")
			require.NoError(t, err)

			assert.Equal(t, tc.Expected, got.Backends)
		})
	}

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().Get(mock.Anything, "nope").
			Return(database.Service{}, database.ErrServiceNotFound).Once()

		svc := newServiceService(t, repo, NewMockWorkloadLister(t))

		_, err := svc.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrServiceNotFound)
	})
}

func TestServiceService_List(t *testing.T) {
	t.Parallel()

	t.Run("resolves backends for every listed service", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return([]database.Service{storedService()}, nil).Once()

		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything, []string{`$.labels."app"=web`}).
			Return([]service.Workload{
				targetedWorkload("web", driver.Instance{Index: 0, State: driver.StateRunning}),
			}, nil).Once()

		svc := newServiceService(t, repo, workloads)

		got, err := svc.List(t.Context())
		require.NoError(t, err)

		require.Len(t, got, 1)
		assert.Equal(t, []service.Backend{
			{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
		}, got[0].Backends)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		t.Parallel()

		svc := newServiceService(t, NewMockServiceRepository(t), NewMockWorkloadLister(t))

		_, err := svc.List(t.Context(), "not-a-query")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})
}

func TestServiceService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes the service", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().Delete(mock.Anything, "example").Return(nil).Once()

		svc := newServiceService(t, repo, NewMockWorkloadLister(t))

		assert.NoError(t, svc.Delete(t.Context(), "example"))
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().Delete(mock.Anything, "nope").Return(database.ErrServiceNotFound).Once()

		svc := newServiceService(t, repo, NewMockWorkloadLister(t))

		assert.ErrorIs(t, svc.Delete(t.Context(), "nope"), service.ErrServiceNotFound)
	})
}
