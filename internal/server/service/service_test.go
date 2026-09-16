package service_test

import (
	"context"
	"errors"
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
// backends at the test host address and following no reconciler.
func newServiceService(t *testing.T, repo *MockServiceRepository, workloads *MockWorkloadLister) *service.ServiceService {
	t.Helper()

	return newStreamingServiceService(t, repo, workloads, nil)
}

// newStreamingServiceService constructs a ServiceService over the given mocks
// that follows the given reconciler's passes.
func newStreamingServiceService(t *testing.T, repo *MockServiceRepository, workloads *MockWorkloadLister, passes *MockPasses) *service.ServiceService {
	t.Helper()

	config := service.ServiceServiceConfig{
		Logger:    slog.New(slog.DiscardHandler),
		Services:  repo,
		Workloads: workloads,
		Address:   "203.0.113.10",
	}

	// Left nil rather than set to a nil pointer, which would satisfy the interface
	// and then be called.
	if passes != nil {
		config.Passes = passes
	}

	return service.NewServiceService(config)
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

// targetedWorkload returns a hydrated workload labelled app=web, which is what
// storedService targets, publishing 8080/tcp for each of the given instances,
// each mapped to a host port of 20000 plus its index.
func targetedWorkload(name string, instances ...driver.Instance) service.Workload {
	workload := service.Workload{Name: name, Labels: map[string]string{"app": "web"}}

	for _, instance := range instances {
		workload.Instances = append(workload.Instances, service.Instance{Instance: instance})
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
					Instances: []service.Instance{{Instance: driver.Instance{Index: 0, State: driver.StateRunning}}},
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
					Instances: []service.Instance{{Instance: driver.Instance{Index: 0, State: driver.StateRunning}}},
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

		// The whole fleet, read once with no query. Selecting happens here rather
		// than in the database, because a read observes the host and one per
		// service would observe it once per service.
		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything).
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

	t.Run("reads the fleet once however many services it resolves", func(t *testing.T) {
		t.Parallel()

		web := storedService()

		api := storedService()
		api.ID, api.Name, api.TargetLabels = "svc-2", "api", map[string]string{"app": "api", "tier": "backend"}

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return([]database.Service{api, web}, nil).Once()

		apiWorkload := targetedWorkload("api", driver.Instance{Index: 0, State: driver.StateRunning})
		apiWorkload.Labels = map[string]string{"app": "api", "tier": "backend", "team": "platform"}

		// Carries one of the two labels the api service wants, which is not
		// enough to be selected by it, and none that the web service wants.
		worker := targetedWorkload("worker", driver.Instance{Index: 0, State: driver.StateRunning})
		worker.Labels = map[string]string{"tier": "backend"}

		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything).
			Return([]service.Workload{
				apiWorkload,
				targetedWorkload("web", driver.Instance{Index: 0, State: driver.StateRunning}),
				worker,
			}, nil).Once()

		svc := newServiceService(t, repo, workloads)

		got, err := svc.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 2)

		// A workload carrying more labels than the target names is still
		// selected, and one missing any of them is not.
		assert.Equal(t, "api", got[0].Name)
		assert.Equal(t, []service.Backend{
			{Workload: "api", Instance: 0, Address: "203.0.113.10:20000"},
		}, got[0].Backends)

		assert.Equal(t, "example", got[1].Name)
		assert.Equal(t, []service.Backend{
			{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
		}, got[1].Backends)
	})

	t.Run("selects every workload for a target naming no labels", func(t *testing.T) {
		t.Parallel()

		everything := storedService()
		everything.TargetLabels = nil

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return([]database.Service{everything}, nil).Once()

		unlabelled := targetedWorkload("plain", driver.Instance{Index: 0, State: driver.StateRunning})
		unlabelled.Labels = nil

		// An empty target selects everything, as the empty query the database
		// would have been asked does.
		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything).Return([]service.Workload{unlabelled}, nil).Once()

		svc := newServiceService(t, repo, workloads)

		got, err := svc.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Len(t, got[0].Backends, 1)
	})

	t.Run("does not observe the host when there are no services", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		// The lister expects no call, so one would fail the test.
		svc := newServiceService(t, repo, NewMockWorkloadLister(t))

		got, err := svc.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("reports a fleet that cannot be read", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return([]database.Service{storedService()}, nil).Once()

		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything).Return(nil, errors.New("database is closed")).Once()

		svc := newServiceService(t, repo, workloads)

		_, err := svc.List(t.Context())
		assert.Error(t, err)
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

func TestServiceService_Stream(t *testing.T) {
	t.Parallel()

	t.Run("refuses a malformed query before subscribing", func(t *testing.T) {
		t.Parallel()

		svc := newStreamingServiceService(t, NewMockServiceRepository(t), NewMockWorkloadLister(t), NewMockPasses(t))

		err := svc.Stream(t.Context(), func([]service.Service) error {
			t.Fatal("nothing should be reported for a query the server refuses")
			return nil
		}, "no-equals")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})

	t.Run("reports the set on subscribing and again when it changes", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return([]database.Service{storedService()}, nil)

		// One instance, then the same instance again, then two. The read that
		// finds nothing new must not be reported.
		fleets := [][]service.Workload{
			{targetedWorkload("web", driver.Instance{Index: 0, State: driver.StateRunning})},
			{targetedWorkload("web", driver.Instance{Index: 0, State: driver.StateRunning})},
			{targetedWorkload("web",
				driver.Instance{Index: 0, State: driver.StateRunning},
				driver.Instance{Index: 1, State: driver.StateRunning},
			)},
		}

		reads := 0
		workloads := NewMockWorkloadLister(t)
		workloads.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context, ...string) ([]service.Workload, error) {
			fleet := fleets[reads]
			reads++

			return fleet, nil
		}).Times(3)

		passes := make(chan struct{})
		reconciler := NewMockPasses(t)
		reconciler.EXPECT().Subscribe(mock.Anything).Return(passes).Once()

		svc := newStreamingServiceService(t, repo, workloads, reconciler)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		reported := make(chan []service.Backend, 3)
		done := make(chan error, 1)

		go func() {
			done <- svc.Stream(ctx, func(services []service.Service) error {
				reported <- services[0].Backends
				return nil
			})
		}()

		assert.Equal(t, []service.Backend{
			{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
		}, <-reported)

		// Two passes: the first finds the same fleet, the second a bigger one.
		// Each send is a synchronous handoff, so the second read has happened
		// before the third pass is offered.
		passes <- struct{}{}
		passes <- struct{}{}

		assert.Equal(t, []service.Backend{
			{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
			{Workload: "web", Instance: 1, Address: "203.0.113.10:20001"},
		}, <-reported)

		cancel()
		assert.NoError(t, <-done)
		assert.Empty(t, reported, "the unchanged read was reported")
	})

	t.Run("ends with the caller's error", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		reconciler := NewMockPasses(t)
		reconciler.EXPECT().Subscribe(mock.Anything).Return(nil).Once()

		svc := newStreamingServiceService(t, repo, NewMockWorkloadLister(t), reconciler)

		err := svc.Stream(t.Context(), func([]service.Service) error {
			return errors.New("hung up")
		})
		assert.EqualError(t, err, "hung up")
	})

	t.Run("ends with the read that failed", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return(nil, nil).Once()
		repo.EXPECT().List(mock.Anything).Return(nil, errors.New("database is gone")).Once()

		passes := make(chan struct{}, 1)
		passes <- struct{}{}

		reconciler := NewMockPasses(t)
		reconciler.EXPECT().Subscribe(mock.Anything).Return(passes).Once()

		svc := newStreamingServiceService(t, repo, NewMockWorkloadLister(t), reconciler)

		err := svc.Stream(t.Context(), func([]service.Service) error { return nil })
		assert.ErrorContains(t, err, "database is gone")
	})

	t.Run("reports once and waits when there is no reconciler to follow", func(t *testing.T) {
		t.Parallel()

		repo := NewMockServiceRepository(t)
		repo.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		svc := newServiceService(t, repo, NewMockWorkloadLister(t))

		ctx, cancel := context.WithCancel(t.Context())

		calls := 0
		err := svc.Stream(ctx, func([]service.Service) error {
			calls++
			cancel()

			return nil
		})
		assert.NoError(t, err)
		assert.Equal(t, 1, calls)
	})
}

func TestServiceService_AsksForAPassWhenAServiceChanges(t *testing.T) {
	t.Parallel()

	repo := NewMockServiceRepository(t)
	repo.EXPECT().Upsert(mock.Anything, mock.Anything).Return(storedService(), true, nil).Once()
	repo.EXPECT().Delete(mock.Anything, "example").Return(nil).Once()

	workloads := NewMockWorkloadLister(t)
	workloads.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()

	// Once per change. A stream learns of a service arriving or going through
	// the pass this asks for.
	reconciler := NewMockPasses(t)
	reconciler.EXPECT().Notify().Twice()

	svc := newStreamingServiceService(t, repo, workloads, reconciler)

	_, _, err := svc.Apply(t.Context(), manifest.Service{
		Version: "v1",
		Name:    "example",
		Target:  manifest.ServiceTarget{Labels: map[string]string{"app": "web"}, Port: 8080},
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(t.Context(), "example"))
}
