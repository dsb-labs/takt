package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestWorkloadService_Apply(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		Spec       api.WorkloadSpec
		SetupMocks func(*MockDriver, *MockWorkloadRepository, *MockPortRepository)
		Assert     func(*testing.T, service.Workload, bool)
		ExpectErr  error
	}{
		{
			Name: "stores a container workload",
			Spec: containerSpec("example", "example/example:latest"),
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository, ports *MockPortRepository) {
				repo.EXPECT().Get(mock.Anything, "example").
					Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

				repo.EXPECT().Upsert(mock.Anything, mock.MatchedBy(func(w database.Workload) bool {
					return w.Name == "example" &&
						w.Runtime == string(api.Container) &&
						w.SpecHash != "" &&
						len(w.Spec) > 0
				})).RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
					w.Version = 1
					return w, true, nil
				}).Once()

				d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, w service.Workload, created bool) {
				assert.True(t, created)
				assert.Equal(t, "example", w.Name)
				assert.Equal(t, 1, w.Version)
				assert.Equal(t, api.Container, w.Runtime)
				// Nothing is running yet, so the workload reads as pending until
				// the reconciler starts it.
				assert.Equal(t, api.WorkloadStatePending, w.State)
			},
		},
		{
			Name: "reports a running workload",
			Spec: containerSpec("example", "example/example:latest"),
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository, ports *MockPortRepository) {
				repo.EXPECT().Get(mock.Anything, "example").
					Return(storedWorkload("example"), nil).Once()

				repo.EXPECT().Upsert(mock.Anything, mock.Anything).
					RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
						w.Version = 2
						return w, false, nil
					}).Once()

				d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
					{ID: "container-one", Workload: "example", State: driver.StateRunning},
				}, nil).Once()
			},
			Assert: func(t *testing.T, w service.Workload, created bool) {
				assert.False(t, created)
				assert.Equal(t, api.WorkloadStateRunning, w.State)
				require.Len(t, w.Instances, 1)
				assert.Equal(t, "container-one", w.Instances[0].ID)
			},
		},
		{
			Name: "rejects an unknown schema version",
			Spec: func() api.WorkloadSpec {
				spec := containerSpec("example", "example/example:latest")
				spec.Version = "v99"
				return spec
			}(),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			Name: "rejects a name the runtime could not represent",
			Spec: containerSpec("BAD_NAME", "example/example:latest"),
			// The CLI checks this, but a caller that skips the CLI must not be able
			// to store a workload whose name breaks orca's own documented rules.
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			Name:       "rejects a container with no image",
			Spec:       containerSpec("example", ""),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			// Storing this would produce a workload that can only ever fail to start.
			ExpectErr: service.ErrInvalidSpec,
		},
		{
			Name: "rejects a schedule that is not cron",
			Spec: func() api.WorkloadSpec {
				spec := containerSpec("example", "example/example:latest")
				spec.Schedule = new("not a cron")
				return spec
			}(),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			Name: "rejects a port outside the usable range",
			Spec: func() api.WorkloadSpec {
				spec := containerSpec("example", "example/example:latest")
				spec.Container.Ports = &[]api.PortMapping{{To: 70000}}
				return spec
			}(),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			Name: "rejects a script workload as unsupported",
			Spec: api.WorkloadSpec{
				Version: "v1",
				Name:    "example",
				Script:  &api.ScriptSpec{Raw: new(`echo "hello world"`)},
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrUnsupportedRuntime,
		},
		{
			Name: "rejects a workload naming no runtime",
			Spec: api.WorkloadSpec{
				Version: "v1",
				Name:    "example",
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrNoRuntime,
		},
		{
			Name: "rejects a workload naming two runtimes",
			Spec: api.WorkloadSpec{
				Version:   "v1",
				Name:      "example",
				Container: &api.ContainerSpec{Image: "example/example:latest"},
				Script:    &api.ScriptSpec{Raw: new("echo hello")},
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrAmbiguousRuntime,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
			tc.SetupMocks(d, repo, ports)

			svc := newTestService(t, d, repo, ports, nil)

			got, created, err := svc.Apply(t.Context(), tc.Spec)
			if tc.ExpectErr != nil {
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			}

			require.NoError(t, err)
			tc.Assert(t, got, created)
		})
	}
}

func TestWorkloadService_Apply_NotifiesReconciler(t *testing.T) {
	t.Parallel()

	d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, "example").
		Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
	repo.EXPECT().Upsert(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
			w.Version = 1
			return w, true, nil
		}).Once()
	d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

	var notified bool
	svc := newTestService(t, d, repo, ports, func() { notified = true })

	_, _, err := svc.Apply(t.Context(), containerSpec("example", "example/example:latest"))
	require.NoError(t, err)

	// Desired state changed, so the reconciler must be woken rather than left to
	// discover the change on its next tick.
	assert.True(t, notified)
}

func TestWorkloadService_Get(t *testing.T) {
	t.Parallel()

	t.Run("merges observed instances into desired state", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", State: driver.StateRunning, SpecHash: "hash-one"},
		}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, api.WorkloadStateRunning, got.State)
		require.Len(t, got.Instances, 1)
	})

	t.Run("still reports desired state when the driver is unreachable", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, errors.New("docker is down")).Once()

		svc := newTestService(t, d, repo, ports, nil)

		// An unreachable runtime shouldn't make a read of desired state fail.
		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Empty(t, got.Instances)
		assert.Equal(t, api.WorkloadStatePending, got.State)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_Get_Health(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name        string
		Result      health.Result
		Checked     bool
		ExpectState api.WorkloadState
	}{
		{
			Name:        "a passing check leaves the workload running",
			Result:      health.Result{Status: health.StatusHealthy},
			Checked:     true,
			ExpectState: api.WorkloadStateRunning,
		},
		{
			Name:    "a check that has not passed yet reads as pending",
			Result:  health.Result{Status: health.StatusStarting},
			Checked: true,
			// A workload still warming up must not be replaced for not having
			// answered yet, so this has to be a state the reconciler treats as up.
			ExpectState: api.WorkloadStatePending,
		},
		{
			Name:    "a failing check makes the workload failed",
			Result:  health.Result{Status: health.StatusUnhealthy, Failures: 3},
			Checked: true,
			// The container is running as far as docker is concerned, but it cannot
			// serve — which is the whole point of checking.
			ExpectState: api.WorkloadStateFailed,
		},
		{
			Name:        "an unchecked workload is unaffected",
			Checked:     false,
			ExpectState: api.WorkloadStateRunning,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
			checker := NewMockChecker(t)

			repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
			ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()
			d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateRunning},
			}, nil).Once()

			checker.EXPECT().Result("example").Return(tc.Result, tc.Checked)

			svc := service.NewWorkloadService(service.WorkloadServiceConfig{
				Logger:    newTestLogger(t),
				Driver:    d,
				Workloads: repo,
				Ports:     ports,
				Allocator: allocatorStub{},
				Checker:   checker,
			})

			got, err := svc.Get(t.Context(), "example")
			require.NoError(t, err)

			assert.Equal(t, tc.ExpectState, got.State)
			assert.Equal(t, tc.Checked, got.Health.Checked)
		})
	}
}

func TestWorkloadService_Get_State(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Instances []driver.Instance
		Expected  api.WorkloadState
	}{
		{
			Name:     "nothing running yet is pending",
			Expected: api.WorkloadStatePending,
		},
		{
			Name: "a container being torn down is terminating",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateTerminating},
			},
			Expected: api.WorkloadStateTerminating,
		},
		{
			Name: "a replacement already up outranks its departing predecessor",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateTerminating},
				{ID: "container-two", Workload: "example", State: driver.StateRunning},
			},
			// The workload is serving traffic, so reporting it as terminating
			// would misrepresent a healthy mid-replacement workload.
			Expected: api.WorkloadStateRunning,
		},
		{
			Name: "teardown is reported ahead of how the instance ended",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateTerminating},
				{ID: "container-two", Workload: "example", State: driver.StateFailed, ExitCode: 137},
			},
			// The non-zero exit is a consequence of the teardown — orca stopped
			// it — rather than news in its own right.
			Expected: api.WorkloadStateTerminating,
		},
		{
			Name: "a failed instance outranks a clean exit",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
				{ID: "container-two", Workload: "example", State: driver.StateFailed, ExitCode: 1},
			},
			Expected: api.WorkloadStateFailed,
		},
		{
			Name: "a clean exit is stopped",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: api.WorkloadStateStopped,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

			repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
			d.EXPECT().Observe(mock.Anything).Return(tc.Instances, nil).Once()

			svc := newTestService(t, d, repo, ports, nil)

			got, err := svc.Get(t.Context(), "example")
			require.NoError(t, err)
			assert.Equal(t, tc.Expected, got.State)
		})
	}
}

func TestWorkloadService_Get_Completion(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Policy    api.RestartPolicy
		Instances []driver.Instance
		Expected  api.WorkloadState
	}{
		{
			// The default policy restarts whatever happened, so a clean exit is a
			// workload waiting to come back rather than one that finished.
			Name:   "a clean exit under always is stopped",
			Policy: api.Always,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: api.WorkloadStateStopped,
		},
		{
			Name:   "a clean exit under on-failure is completed",
			Policy: api.OnFailure,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: api.WorkloadStateCompleted,
		},
		{
			// Retired, but not a success. Reporting this as completed would tell an
			// operator the job did its work when it did not.
			Name:   "a failure under never is still failed",
			Policy: api.Never,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateFailed, ExitCode: 1},
			},
			Expected: api.WorkloadStateFailed,
		},
		{
			Name:   "a clean exit under never is completed",
			Policy: api.Never,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: api.WorkloadStateCompleted,
		},
		{
			// Completion must not mask a problem: the operator needs the failure
			// first, and the completion is true but not the news.
			Name:   "a failed instance outranks a completed one",
			Policy: api.OnFailure,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
				{ID: "container-two", Workload: "example", State: driver.StateFailed, ExitCode: 1},
			},
			Expected: api.WorkloadStateFailed,
		},
		{
			Name:   "a running instance outranks a completed one",
			Policy: api.OnFailure,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
				{ID: "container-two", Workload: "example", State: driver.StateRunning},
			},
			Expected: api.WorkloadStateRunning,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

			row := storedWorkload("example")
			spec := containerSpec("example", "example/example:latest")
			spec.Restart = new(tc.Policy)

			encoded, err := json.Marshal(spec)
			require.NoError(t, err)
			row.Spec = encoded

			repo.EXPECT().Get(mock.Anything, "example").Return(row, nil).Once()
			d.EXPECT().Observe(mock.Anything).Return(tc.Instances, nil).Once()

			svc := newTestService(t, d, repo, ports, nil)

			got, err := svc.Get(t.Context(), "example")
			require.NoError(t, err)
			assert.Equal(t, tc.Expected, got.State)
		})
	}
}

func TestWorkloadService_List(t *testing.T) {
	t.Parallel()

	t.Run("observes the driver once for every workload", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{
			storedWorkload("alpha"),
			storedWorkload("bravo"),
		}, nil).Once()

		// One observation covers every workload; asking per row would scale badly.
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "alpha", State: driver.StateRunning},
		}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, api.WorkloadStateRunning, got[0].State)
		assert.Equal(t, api.WorkloadStatePending, got[1].State)
	})
}

func TestWorkloadService_Apply_PortCollision(t *testing.T) {
	t.Parallel()

	t.Run("allocates again when another workload claims the port first", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil)
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		// Two applies racing each other can pick the same free port, and the unique
		// constraint means one loses. Since orca chose the port, losing is its
		// problem to resolve rather than something to report to the caller.
		var attempts int
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				attempts++
				if attempts == 1 {
					return database.Workload{}, false, database.ErrHostPortTaken
				}

				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		svc := newTestService(t, d, repo, ports, nil)

		_, _, err := svc.Apply(t.Context(), containerSpec("example", "example/example:latest"))
		require.NoError(t, err)
		assert.Equal(t, 2, attempts)
	})

	t.Run("reports a pinned port that collides", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			Return(database.Workload{}, false, database.ErrHostPortTaken)
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		ports.EXPECT().HolderOf(mock.Anything, 4141).Return("", false, nil)

		svc := newTestService(t, d, repo, ports, nil)

		// The caller asked for this port specifically, so retrying would be picking
		// a different one behind their back.
		spec := containerSpec("example", "example/example:latest")
		spec.Container.Ports = &[]api.PortMapping{{To: 8080, From: new(4141)}}

		_, _, err := svc.Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrHostPortTaken)
	})
}

func TestWorkloadService_Apply_NoPortsAvailable(t *testing.T) {
	t.Parallel()

	d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, "example").
		Return(database.Workload{}, database.ErrWorkloadNotFound)
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)

	svc := service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Driver:    d,
		Workloads: repo,
		Ports:     ports,
		Allocator: allocatorStub{err: port.ErrRangeExhausted},
	})

	// A workload has to actually want a host port for allocation to be reached.
	spec := containerSpec("example", "example/example:latest")
	spec.Container.Ports = &[]api.PortMapping{{To: 8080}}

	// An exhausted range is a capacity problem rather than a fault or a bad request,
	// and the API depends on this translation to answer 503 rather than 500.
	_, _, err := svc.Apply(t.Context(), spec)
	assert.ErrorIs(t, err, service.ErrNoPortsAvailable)

	// The workload must not have been stored: it has no reachable address, and the
	// caller was told the apply failed.
	repo.AssertNotCalled(t, "Upsert")
}

func TestWorkloadService_Get_DriverHangs(t *testing.T) {
	t.Parallel()

	d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()

	// A daemon that accepts the call and never answers is the case a timeout exists
	// for: the observation is bounded by a context, so it ends when that context does
	// rather than when the driver decides to reply.
	d.EXPECT().Observe(mock.Anything).RunAndReturn(func(ctx context.Context) ([]driver.Instance, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}).Once()

	svc := service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Driver:    d,
		Workloads: repo,
		Ports:     ports,
		Allocator: allocatorStub{},
	})

	// The caller's own deadline is shorter than the service's, so this proves the
	// observation honours the context it is given rather than ignoring cancellation.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	// Desired state is still reported: an unreachable runtime makes a read less
	// informative, not a failure.
	got, err := svc.Get(ctx, "example")
	require.NoError(t, err)

	assert.Equal(t, "example", got.Name)
	assert.Empty(t, got.Instances)
	assert.Equal(t, api.WorkloadStatePending, got.State)
}

func TestWorkloadService_List_Queries(t *testing.T) {
	t.Parallel()

	t.Run("passes parsed queries to the repository", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().List(mock.Anything, []database.Query{{Path: "$.labels.app", Value: "web"}}).
			Return([]database.Workload{storedWorkload("example")}, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.List(t.Context(), "$.labels.app=web")
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("keeps an equals sign in the value", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		// Only the first equals separates path from value, since a value may well
		// contain one of its own.
		repo.EXPECT().List(mock.Anything, []database.Query{{Path: "$.labels.expr", Value: "a=b"}}).
			Return(nil, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.List(t.Context(), "$.labels.expr=a=b")
		require.NoError(t, err)
	})

	t.Run("rejects a query that is not path=value", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.List(t.Context(), "$.labels.app")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})

	t.Run("reports a path the repository cannot parse", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, database.ErrInvalidQueryPath).Once()

		svc := newTestService(t, d, repo, ports, nil)

		// A bad path is the caller's mistake, so it must not surface as a server
		// failure.
		_, err := svc.List(t.Context(), "nonsense=web")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})
}

func TestWorkloadService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("marks the workload for deletion without touching the runtime", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		marked := storedWorkload("example")
		marked.DeletedAt = time.Now().UTC()

		repo.EXPECT().MarkDeleting(mock.Anything, "example").Return(marked, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", State: driver.StateRunning},
		}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.Delete(t.Context(), "example")
		require.NoError(t, err)

		// The reconciler owns stopping the work, so the service records the intent
		// and reports the workload as on its way out. A running instance does not
		// make it read as running any more.
		assert.True(t, got.Deleting)
		assert.Equal(t, api.WorkloadStateTerminating, got.State)
	})

	t.Run("notifies the reconciler", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().MarkDeleting(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		var notified bool
		svc := newTestService(t, d, repo, ports, func() { notified = true })

		_, err := svc.Delete(t.Context(), "example")
		require.NoError(t, err)

		// Nothing happens until the reconciler runs, so it has to be woken.
		assert.True(t, notified)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().MarkDeleting(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Delete(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_Apply_RejectsATerminatingWorkload(t *testing.T) {
	t.Parallel()

	d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	deleting := storedWorkload("example")
	deleting.DeletedAt = time.Now().UTC()

	repo.EXPECT().Get(mock.Anything, "example").Return(deleting, nil).Once()

	svc := newTestService(t, d, repo, ports, nil)

	// Re-applying a workload mid-teardown would race the reconciler removing it,
	// and could leave the freshly applied instance being torn down instead.
	_, _, err := svc.Apply(t.Context(), containerSpec("example", "example/example:latest"))
	assert.ErrorIs(t, err, service.ErrWorkloadDeleting)
}

func TestWorkloadService_Logs(t *testing.T) {
	t.Parallel()

	t.Run("returns the driver's output", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Logs(mock.Anything, mock.Anything, "example", 20).
			RunAndReturn(func(_ context.Context, out io.Writer, _ string, _ int) error {
				_, err := out.Write([]byte("hello world\n"))
				return err
			}).Once()

		svc := newTestService(t, d, repo, ports, nil)

		var out strings.Builder
		require.NoError(t, svc.Logs(t.Context(), &out, "example", 20))
		assert.Equal(t, "hello world\n", out.String())
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := NewMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		err := svc.Logs(t.Context(), io.Discard, "nope", 20)
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

// newTestService builds a service whose port repository answers the reads every path
// makes, so that a test only has to set up the behaviour it is actually about.
func newTestService(t *testing.T, d *MockDriver, repo *MockWorkloadRepository, ports *MockPortRepository, notify func()) *service.WorkloadService {
	t.Helper()

	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().ListAll(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().Claim(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	ports.EXPECT().HolderOf(mock.Anything, mock.Anything).Return("", false, nil).Maybe()

	return service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Driver:    d,
		Workloads: repo,
		Ports:     ports,
		Allocator: allocatorStub{},
		Notify:    notify,
	})
}

// The allocatorStub type hands out ports from a fixed base, so a test can predict
// what a workload will be allocated without standing up the real allocator.
type allocatorStub struct {
	// err is returned instead of a port, so a test can exercise what the service
	// does when the range has nothing left.
	err error
}

func (a allocatorStub) Allocate(taken []int) (int, error) {
	if a.err != nil {
		return 0, a.err
	}

	return 20000 + len(taken), nil
}

func containerSpec(name, image string) api.WorkloadSpec {
	return api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Container: &api.ContainerSpec{Image: image},
	}
}

func storedWorkload(name string) database.Workload {
	spec, err := json.Marshal(containerSpec(name, "example/example:latest"))
	if err != nil {
		panic(err)
	}

	return database.Workload{
		Name:     name,
		Version:  1,
		Runtime:  string(api.Container),
		Spec:     spec,
		SpecHash: "hash-one",
	}
}

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}
