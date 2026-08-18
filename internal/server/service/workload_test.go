package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestWorkloadService_Apply(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		Spec       api.WorkloadSpec
		SetupMocks func(*MockDriver, *MockWorkloadRepository)
		Assert     func(*testing.T, service.Workload, bool)
		ExpectErr  error
	}{
		{
			Name: "stores a container workload",
			Spec: containerSpec("example", "example/example:latest"),
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().Upsert(mock.Anything, mock.MatchedBy(func(w database.Workload) bool {
					return w.Name == "example" &&
						w.Runtime == string(api.Container) &&
						w.SpecHash != "" &&
						len(w.Spec) > 0
				})).RunAndReturn(func(_ context.Context, w database.Workload) (database.Workload, bool, error) {
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
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().Upsert(mock.Anything, mock.Anything).
					RunAndReturn(func(_ context.Context, w database.Workload) (database.Workload, bool, error) {
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
			Name: "rejects a script workload as unsupported",
			Spec: api.WorkloadSpec{
				Version: "v1",
				Name:    "example",
				Script:  &api.ScriptSpec{Raw: new(`echo "hello world"`)},
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository) {},
			ExpectErr:  service.ErrUnsupportedRuntime,
		},
		{
			Name: "rejects a workload naming no runtime",
			Spec: api.WorkloadSpec{
				Version: "v1",
				Name:    "example",
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository) {},
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
			SetupMocks: func(*MockDriver, *MockWorkloadRepository) {},
			ExpectErr:  service.ErrAmbiguousRuntime,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)
			tc.SetupMocks(d, repo)

			svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

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

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().Upsert(mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, w database.Workload) (database.Workload, bool, error) {
			w.Version = 1
			return w, true, nil
		}).Once()
	d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

	var notified bool
	svc := service.NewWorkloadService(newTestLogger(t), d, repo, func() { notified = true })

	_, _, err := svc.Apply(t.Context(), containerSpec("example", "example/example:latest"))
	require.NoError(t, err)

	// Desired state changed, so the reconciler must be woken rather than left to
	// discover the change on its next tick.
	assert.True(t, notified)
}

func TestWorkloadService_Get(t *testing.T) {
	t.Parallel()

	t.Run("merges observed instances into desired state", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", State: driver.StateRunning, SpecHash: "hash-one"},
		}, nil).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, api.WorkloadStateRunning, got.State)
		require.Len(t, got.Instances, 1)
	})

	t.Run("still reports desired state when the driver is unreachable", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, errors.New("docker is down")).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		// An unreachable runtime shouldn't make a read of desired state fail.
		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Empty(t, got.Instances)
		assert.Equal(t, api.WorkloadStatePending, got.State)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		_, err := svc.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_List(t *testing.T) {
	t.Parallel()

	t.Run("observes the driver once for every workload", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{
			storedWorkload("alpha"),
			storedWorkload("bravo"),
		}, nil).Once()

		// One observation covers every workload; asking per row would scale badly.
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "alpha", State: driver.StateRunning},
		}, nil).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		got, err := svc.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, api.WorkloadStateRunning, got[0].State)
		assert.Equal(t, api.WorkloadStatePending, got[1].State)
	})
}

func TestWorkloadService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("stops the driver before removing desired state", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Stop(mock.Anything, "example").Return(nil).Once()
		repo.EXPECT().Delete(mock.Anything, "example").Return(nil).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		require.NoError(t, svc.Delete(t.Context(), "example"))
	})

	t.Run("keeps desired state when the driver cannot be stopped", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Stop(mock.Anything, "example").Return(errors.New("docker is down")).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		// Deleting the row here would orphan a running container that nothing
		// records any more, so the failure has to propagate.
		assert.Error(t, svc.Delete(t.Context(), "example"))
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		assert.ErrorIs(t, svc.Delete(t.Context(), "nope"), service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_Logs(t *testing.T) {
	t.Parallel()

	t.Run("returns the driver's output", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Logs(mock.Anything, "example", 20).Return("hello world\n", nil).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		logs, err := svc.Logs(t.Context(), "example", 20)
		require.NoError(t, err)
		assert.Equal(t, "hello world\n", logs)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := service.NewWorkloadService(newTestLogger(t), d, repo, nil)

		_, err := svc.Logs(t.Context(), "nope", 20)
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
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
