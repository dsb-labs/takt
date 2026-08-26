package service_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/driver/docker"
	"github.com/dsb-labs/orca/internal/server/health"
	"github.com/dsb-labs/orca/internal/server/port"
	"github.com/dsb-labs/orca/internal/server/service"
	"github.com/dsb-labs/orca/pkg/manifest"
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
				assert.Equal(t, manifest.RuntimeContainer, w.Runtime)
				// Nothing is running yet, so the workload reads as pending until
				// the reconciler starts it.
				assert.Equal(t, service.WorkloadStatePending, w.State)
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
				assert.Equal(t, service.WorkloadStateRunning, w.State)
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
				spec.Schedule = &api.ScheduleSpec{Cron: "not a cron"}
				return spec
			}(),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			Name: "rejects a port outside the usable range",
			Spec: func() api.WorkloadSpec {
				spec := containerSpec("example", "example/example:latest")
				spec.Ports = &[]api.PortMapping{{To: 70000}}
				return spec
			}(),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			// The CLI rejects this too, but a caller that skips the CLI must not be
			// able to store limits the exec runtime would silently never apply.
			Name: "rejects resource limits on an exec workload",
			Spec: api.WorkloadSpec{
				Version:   "v1",
				Name:      "example",
				Resources: &api.ResourcesSpec{Memory: new("512m")},
				Exec:      &api.ExecSpec{Command: []string{"echo", "hello world"}},
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			// The CLI checks this, but a caller that skips the CLI must not be able
			// to store a label key orca's own documented rules refuse.
			Name: "rejects a label using the reserved orca. prefix",
			Spec: func() api.WorkloadSpec {
				spec := containerSpec("example", "example/example:latest")
				spec.Labels = &map[string]string{"orca.workload": "spoof"}
				return spec
			}(),
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrInvalidSpec,
		},
		{
			// Accepted rather than refused, which is what the exec runtime arriving
			// means: the service records desired state and the reconciler routes it to
			// whichever driver runs it.
			Name: "accepts an exec workload",
			Spec: api.WorkloadSpec{
				Version: "v1",
				Name:    "example",
				Exec:    &api.ExecSpec{Command: []string{"echo", "hello world"}},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository, _ *MockPortRepository) {
				repo.EXPECT().Get(mock.Anything, "example").
					Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

				repo.EXPECT().Upsert(mock.Anything, mock.Anything).
					RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
						w.Version = 1

						return w, true, nil
					}).Once()

				d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, w service.Workload, created bool) {
				assert.True(t, created)
				assert.Equal(t, manifest.RuntimeExec, w.Runtime)
			},
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
				Exec:      &api.ExecSpec{Command: []string{"echo", "hello"}},
			},
			SetupMocks: func(*MockDriver, *MockWorkloadRepository, *MockPortRepository) {},
			ExpectErr:  service.ErrAmbiguousRuntime,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
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

func TestWorkloadService_Apply_ResolvesVolumes(t *testing.T) {
	t.Parallel()

	spec := containerSpec("example", "example/example:latest")
	spec.Volumes = &[]api.VolumeMount{{Name: new("example-data"), To: "/var/lib/example"}}

	t.Run("stores where each mounted volume lives", func(t *testing.T) {
		t.Parallel()

		// The driver is handed a path rather than a name to look up, and the path is
		// part of what gets hashed, so a volume that moved replaces the instances
		// bound to where it was.
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		volumes := NewMockVolumeLocator(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		volumes.EXPECT().Path(mock.Anything, "example-data").
			Return("/var/lib/orca/volumes/cvhs0dq0kqj4c9r8m1a0", nil).Once()

		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Maybe()
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		repo.EXPECT().Upsert(mock.Anything, mock.MatchedBy(func(w database.Workload) bool {
			var stored api.WorkloadSpec
			if err := json.Unmarshal(w.Spec, &stored); err != nil || stored.Volumes == nil {
				return false
			}

			mounts := *stored.Volumes

			return len(mounts) == 1 &&
				mounts[0].From != nil &&
				*mounts[0].From == "/var/lib/orca/volumes/cvhs0dq0kqj4c9r8m1a0"
		})).RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
			w.Version = 1

			return w, true, nil
		}).Once()

		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		svc := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:    newTestLogger(t),
			Drivers:   map[string]service.Driver{docker.Name: d},
			Workloads: repo,
			Ports:     ports,
			Volumes:   volumes,
			Allocator: allocatorStub{},
		})

		_, _, err := svc.Apply(t.Context(), spec)
		require.NoError(t, err)
	})

	t.Run("refuses a workload naming a volume that does not exist", func(t *testing.T) {
		t.Parallel()

		// Creating it instead would make a mistyped name a second empty volume, which
		// reads as success while the data the workload wanted sits under the name that
		// was meant. Nothing is stored, so the reconciler never sees it.
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		volumes := NewMockVolumeLocator(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		volumes.EXPECT().Path(mock.Anything, "example-data").
			Return("", service.ErrVolumeNotFound).Once()

		repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		svc := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:    newTestLogger(t),
			Drivers:   map[string]service.Driver{docker.Name: d},
			Workloads: repo,
			Ports:     ports,
			Volumes:   volumes,
			Allocator: allocatorStub{},
		})

		_, _, err := svc.Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrVolumeNotFound)
	})

	t.Run("refuses a mount when the server holds no volumes at all", func(t *testing.T) {
		t.Parallel()

		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		svc := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:    newTestLogger(t),
			Drivers:   map[string]service.Driver{docker.Name: d},
			Workloads: repo,
			Ports:     ports,
			Allocator: allocatorStub{},
		})

		_, _, err := svc.Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrVolumeNotFound)
	})
}

func TestWorkloadService_Apply_NotifiesReconciler(t *testing.T) {
	t.Parallel()

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", State: driver.StateRunning, SpecHash: "hash-one"},
		}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, service.WorkloadStateRunning, got.State)
		require.Len(t, got.Instances, 1)
	})

	t.Run("still reports desired state when the driver is unreachable", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, errors.New("docker is down")).Once()

		svc := newTestService(t, d, repo, ports, nil)

		// An unreachable runtime shouldn't make a read of desired state fail.
		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Empty(t, got.Instances)
		assert.Equal(t, service.WorkloadStatePending, got.State)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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
		ExpectState service.WorkloadState
	}{
		{
			Name:        "a passing check leaves the workload running",
			Result:      health.Result{Status: health.StatusHealthy},
			Checked:     true,
			ExpectState: service.WorkloadStateRunning,
		},
		{
			Name:    "a check that has not passed yet reads as pending",
			Result:  health.Result{Status: health.StatusStarting},
			Checked: true,
			// A workload still warming up must not be replaced for not having
			// answered yet, so this has to be a state the reconciler treats as up.
			ExpectState: service.WorkloadStatePending,
		},
		{
			Name:    "a failing check makes the workload failed",
			Result:  health.Result{Status: health.StatusUnhealthy, Failures: 3},
			Checked: true,
			// The container is running as far as docker is concerned, but it cannot
			// serve — which is the whole point of checking.
			ExpectState: service.WorkloadStateFailed,
		},
		{
			Name:        "an unchecked workload is unaffected",
			Checked:     false,
			ExpectState: service.WorkloadStateRunning,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
			checker := NewMockChecker(t)

			repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
			ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()
			d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateRunning},
			}, nil).Once()

			checker.EXPECT().Result("example").Return(tc.Result, tc.Checked)

			repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

			svc := service.NewWorkloadService(service.WorkloadServiceConfig{
				Logger:    newTestLogger(t),
				Drivers:   map[string]service.Driver{docker.Name: d},
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

func TestWorkloadService_Get_LastError(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	tt := []struct {
		Name     string
		Message  string
		At       time.Time
		Recorded bool
	}{
		{
			Name:     "a failing workload reports why",
			Message:  "failed to start workload: no such image",
			At:       when,
			Recorded: true,
		},
		{
			Name: "a converging workload reports nothing",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
			rec := NewMockReconciler(t)

			repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
			ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()
			d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

			rec.EXPECT().LastError("example").Return(tc.Message, tc.At, tc.Recorded)

			repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

			svc := service.NewWorkloadService(service.WorkloadServiceConfig{
				Logger:     newTestLogger(t),
				Drivers:    map[string]service.Driver{docker.Name: d},
				Workloads:  repo,
				Ports:      ports,
				Allocator:  allocatorStub{},
				Reconciler: rec,
			})

			got, err := svc.Get(t.Context(), "example")
			require.NoError(t, err)

			assert.Equal(t, tc.Message, got.LastError)
			assert.Equal(t, tc.At, got.LastErrorAt)
		})
	}

	t.Run("a service with no error source reports nothing", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		svc := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:    newTestLogger(t),
			Drivers:   map[string]service.Driver{docker.Name: d},
			Workloads: repo,
			Ports:     ports,
			Allocator: allocatorStub{},
		})

		got, err := svc.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Empty(t, got.LastError)
		assert.True(t, got.LastErrorAt.IsZero())
	})
}

func TestWorkloadService_Get_State(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Instances []driver.Instance
		Expected  service.WorkloadState
	}{
		{
			Name:     "nothing running yet is pending",
			Expected: service.WorkloadStatePending,
		},
		{
			Name: "a container being torn down is terminating",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateTerminating},
			},
			Expected: service.WorkloadStateTerminating,
		},
		{
			Name: "a replacement already up outranks its departing predecessor",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateTerminating},
				{ID: "container-two", Workload: "example", State: driver.StateRunning},
			},
			// The workload is serving traffic, so reporting it as terminating
			// would misrepresent a healthy mid-replacement workload.
			Expected: service.WorkloadStateRunning,
		},
		{
			Name: "teardown is reported ahead of how the instance ended",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateTerminating},
				{ID: "container-two", Workload: "example", State: driver.StateFailed, ExitCode: 137},
			},
			// The non-zero exit is a consequence of the teardown — orca stopped
			// it — rather than news in its own right.
			Expected: service.WorkloadStateTerminating,
		},
		{
			Name: "a failed instance outranks a clean exit",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
				{ID: "container-two", Workload: "example", State: driver.StateFailed, ExitCode: 1},
			},
			Expected: service.WorkloadStateFailed,
		},
		{
			Name: "a clean exit is stopped",
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: service.WorkloadStateStopped,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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
		Expected  service.WorkloadState
	}{
		{
			// The default policy restarts whatever happened, so a clean exit is a
			// workload waiting to come back rather than one that finished.
			Name:   "a clean exit under always is stopped",
			Policy: api.RestartPolicyAlways,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: service.WorkloadStateStopped,
		},
		{
			Name:   "a clean exit under on-failure is completed",
			Policy: api.RestartPolicyOnFailure,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: service.WorkloadStateCompleted,
		},
		{
			// Retired, but not a success. Reporting this as completed would tell an
			// operator the job did its work when it did not.
			Name:   "a failure under never is still failed",
			Policy: api.RestartPolicyNever,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateFailed, ExitCode: 1},
			},
			Expected: service.WorkloadStateFailed,
		},
		{
			Name:   "a clean exit under never is completed",
			Policy: api.RestartPolicyNever,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
			},
			Expected: service.WorkloadStateCompleted,
		},
		{
			// Completion must not mask a problem: the operator needs the failure
			// first, and the completion is true but not the news.
			Name:   "a failed instance outranks a completed one",
			Policy: api.RestartPolicyOnFailure,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
				{ID: "container-two", Workload: "example", State: driver.StateFailed, ExitCode: 1},
			},
			Expected: service.WorkloadStateFailed,
		},
		{
			Name:   "a running instance outranks a completed one",
			Policy: api.RestartPolicyOnFailure,
			Instances: []driver.Instance{
				{ID: "container-one", Workload: "example", State: driver.StateExited},
				{ID: "container-two", Workload: "example", State: driver.StateRunning},
			},
			Expected: service.WorkloadStateRunning,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

			row := storedWorkload("example")
			spec := containerSpec("example", "example/example:latest")
			spec.Restart = &api.RestartSpec{Policy: new(tc.Policy)}

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

func TestWorkloadService_Get_NextRun(t *testing.T) {
	t.Parallel()

	ran := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	applied := time.Date(2026, 2, 20, 2, 0, 0, 0, time.UTC)

	tt := []struct {
		Name      string
		Cron      string
		Instances []driver.Instance
		Expected  time.Time
	}{
		{
			// A daily expression names one time a day, so the occurrence after a run
			// is the same time the next day.
			Name: "reports the occurrence after the last run",
			Cron: "0 2 * * *",
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", State: driver.StateExited, StartedAt: ran},
			},
			Expected: ran.Add(24 * time.Hour),
		},
		{
			// Nothing has run, so the occurrence is counted from when the
			// specification was applied, which is what the reconciler waits for.
			Name:     "reports the first occurrence for a schedule that has not run",
			Cron:     "0 2 * * *",
			Expected: applied.Add(24 * time.Hour),
		},
		{
			Name: "reports nothing for a workload that runs continuously",
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", State: driver.StateRunning, StartedAt: ran},
			},
			Expected: time.Time{},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

			row := storedWorkload("example")
			if tc.Cron != "" {
				spec := containerSpec("example", "example/example:latest")
				spec.Schedule = &api.ScheduleSpec{Cron: tc.Cron}

				encoded, err := json.Marshal(spec)
				require.NoError(t, err)
				row.Spec = encoded
			}

			row.UpdatedAt = applied

			repo.EXPECT().Get(mock.Anything, "example").Return(row, nil).Once()
			d.EXPECT().Observe(mock.Anything).Return(tc.Instances, nil).Once()

			svc := newTestService(t, d, repo, ports, nil)

			got, err := svc.Get(t.Context(), "example")
			require.NoError(t, err)
			assert.Equal(t, tc.Expected, got.NextRun)
		})
	}
}

func TestWorkloadService_List(t *testing.T) {
	t.Parallel()

	t.Run("observes the driver once for every workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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

		assert.Equal(t, service.WorkloadStateRunning, got[0].State)
		assert.Equal(t, service.WorkloadStatePending, got[1].State)
	})
}

func TestWorkloadService_Apply_PortCollision(t *testing.T) {
	t.Parallel()

	t.Run("allocates again when another workload claims the port first", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			Return(database.Workload{}, false, database.ErrHostPortTaken)
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		ports.EXPECT().HolderOf(mock.Anything, 4141, mock.Anything).Return("", false, nil)

		svc := newTestService(t, d, repo, ports, nil)

		// The caller asked for this port specifically, so retrying would be picking
		// a different one behind their back.
		spec := containerSpec("example", "example/example:latest")
		spec.Ports = &[]api.PortMapping{{To: 8080, From: new(4141)}}

		_, _, err := svc.Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrHostPortTaken)
	})
}

func TestWorkloadService_Apply_WorkloadReferences(t *testing.T) {
	t.Parallel()

	referencing := func(value string) api.WorkloadSpec {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = &map[string]string{"DSN": value}

		return spec
	}

	t.Run("records the workloads a specification references", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		addresses := NewMockWorkloadAddresses(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		addresses.EXPECT().
			Address(mock.Anything, manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "pg"}).
			Return("10.0.0.5:20432", nil).Once()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w
				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		_, _, err := newTestAddressAwareService(t, d, repo, ports, addresses).
			Apply(t.Context(), referencing("postgres://app@${workload:postgres:pg}/app"))
		require.NoError(t, err)

		// Recorded so that moving the referenced workload's ports can find what reads
		// them without parsing every stored specification.
		assert.Equal(t, []string{"postgres"}, stored.Workloads)

		// The reference is stored as written. Storing the address would leave the
		// workload holding one that has since moved.
		assert.Contains(t, string(stored.Spec), "${workload:postgres:pg}")
	})

	t.Run("moves the hash when the referenced address moves", func(t *testing.T) {
		hashes := make([]string, 0, 2)

		for _, address := range []string{"10.0.0.5:20432", "10.0.0.5:20500"} {
			d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
			addresses := NewMockWorkloadAddresses(t)

			repo.EXPECT().Get(mock.Anything, "example").
				Return(database.Workload{}, database.ErrWorkloadNotFound)
			d.EXPECT().Observe(mock.Anything).Return(nil, nil)
			addresses.EXPECT().Address(mock.Anything, mock.Anything).Return(address, nil).Once()

			repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
				RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
					hashes = append(hashes, w.SpecHash)
					w.ID, w.Version = "id-one", 1
					return w, true, nil
				})

			_, _, err := newTestAddressAwareService(t, d, repo, ports, addresses).
				Apply(t.Context(), referencing("postgres://app@${workload:postgres:pg}/app"))
			require.NoError(t, err)
		}

		// The address a workload was started with is part of what it is, so a host
		// port that was reallocated has to replace the instances reading it.
		require.Len(t, hashes, 2)
		assert.NotEqual(t, hashes[0], hashes[1])
	})

	t.Run("refuses a workload that does not exist", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		addresses := NewMockWorkloadAddresses(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)

		addresses.EXPECT().Address(mock.Anything, mock.Anything).
			Return("", fmt.Errorf("%w: nope", service.ErrWorkloadNotFound)).Once()

		// The workload could never run, and the operator asking for it is the one who
		// can fix the name or apply what it names first.
		_, _, err := newTestAddressAwareService(t, d, repo, ports, addresses).
			Apply(t.Context(), referencing("${workload:nope}"))
		require.ErrorIs(t, err, service.ErrWorkloadNotFound)
		assert.Contains(t, err.Error(), "nope")

		repo.AssertNotCalled(t, "Upsert")
	})

	t.Run("refuses a port the referenced workload does not publish", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		addresses := NewMockWorkloadAddresses(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)

		addresses.EXPECT().Address(mock.Anything, mock.Anything).
			Return("", fmt.Errorf("%w: workload postgres does not publish http", service.ErrPortNotPublished)).Once()

		_, _, err := newTestAddressAwareService(t, d, repo, ports, addresses).
			Apply(t.Context(), referencing("${workload:postgres:http}"))
		require.ErrorIs(t, err, service.ErrPortNotPublished)
		assert.Contains(t, err.Error(), "postgres:http")

		repo.AssertNotCalled(t, "Upsert")
	})

	t.Run("refuses a reference on a server resolving no addresses", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)

		_, _, err := newTestService(t, d, repo, ports, nil).
			Apply(t.Context(), referencing("${workload:postgres}"))
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})

	t.Run("hashes a workload referencing none as it did before", func(t *testing.T) {
		// A workload reading nothing must encode exactly as it always has, or an
		// upgrade would replace every running instance.
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w
				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		_, _, err := newTestService(t, d, repo, ports, nil).
			Apply(t.Context(), containerSpec("example", "example/example:latest"))
		require.NoError(t, err)

		sum := sha256.Sum256(stored.Spec)
		assert.Equal(t, hex.EncodeToString(sum[:]), stored.SpecHash)
		assert.Empty(t, stored.Workloads)
	})
}

func TestWorkloadService_Apply_Ports(t *testing.T) {
	t.Parallel()

	t.Run("claims a udp port against the udp address space", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		// What is taken over TCP says nothing about UDP, so an allocation that read
		// one set of ports would refuse a port that is genuinely free.
		ports.EXPECT().Allocated(mock.Anything).Return(map[string][]int{"tcp": {20000, 20001}}, nil)

		var claimed []database.Port
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, p ...database.Port) (database.Workload, bool, error) {
				claimed = p
				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		svc := newTestService(t, d, repo, ports, nil)

		spec := containerSpec("example", "example/example:latest")
		spec.Ports = &[]api.PortMapping{{To: 53, Protocol: new(api.UDP)}}

		_, _, err := svc.Apply(t.Context(), spec)
		require.NoError(t, err)

		require.Len(t, claimed, 1)
		assert.Equal(t, 53, claimed[0].Container)
		assert.Equal(t, string(api.UDP), claimed[0].Protocol)
		assert.Equal(t, 20000, claimed[0].Host)
	})

	t.Run("gives one port published on both protocols the same host port", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		var claimed []database.Port
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, p ...database.Port) (database.Workload, bool, error) {
				claimed = p
				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		svc := newTestService(t, d, repo, ports, nil)

		// A DNS server answering on 20000/udp and 20014/tcp reads as an accident, so
		// the two allocations are made together.
		spec := containerSpec("example", "example/example:latest")
		spec.Ports = &[]api.PortMapping{
			{To: 53, Protocol: new(api.TCP)},
			{To: 53, Protocol: new(api.UDP)},
		}

		_, _, err := svc.Apply(t.Context(), spec)
		require.NoError(t, err)

		require.Len(t, claimed, 2)
		assert.Equal(t, string(api.TCP), claimed[0].Protocol)
		assert.Equal(t, string(api.UDP), claimed[1].Protocol)
		assert.Equal(t, claimed[0].Host, claimed[1].Host)
	})

	t.Run("claims a port under the name the specification gave it", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		var claimed []database.Port
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, p ...database.Port) (database.Workload, bool, error) {
				claimed = p
				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		ports.EXPECT().List(mock.Anything, "id-one").
			RunAndReturn(func(context.Context, string) ([]database.Port, error) { return claimed, nil })

		svc := newTestService(t, d, repo, ports, nil)

		spec := containerSpec("example", "example/example:latest")
		spec.Ports = &[]api.PortMapping{{Name: new("http"), To: 8080}, {To: 9090}}

		workload, _, err := svc.Apply(t.Context(), spec)
		require.NoError(t, err)

		require.Len(t, claimed, 2)
		assert.Equal(t, "http", claimed[0].Name)
		assert.Empty(t, claimed[1].Name)

		// Reported back as well, since the name is how a caller tells one of a
		// workload's addresses from another.
		require.Len(t, workload.Ports, 2)
		assert.Equal(t, "http", workload.Ports[0].Name)
		assert.Empty(t, workload.Ports[1].Name)
	})

	t.Run("keeps the host port when a port is renamed", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "id-one", Name: "example", Version: 1}, nil)
		ports.EXPECT().List(mock.Anything, "id-one").
			Return([]database.Port{{Name: "http", Container: 8080, Host: 20000, Protocol: "tcp", Dynamic: true}}, nil)
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		var claimed []database.Port
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, p ...database.Port) (database.Workload, bool, error) {
				claimed = p
				w.ID, w.Version = "id-one", 2
				return w, false, nil
			})

		svc := newTestService(t, d, repo, ports, nil)

		// Renaming a port says what the specification calls it, not where it is
		// reached, so the address the workload already had must not move.
		spec := containerSpec("example", "example/example:latest")
		spec.Ports = &[]api.PortMapping{{Name: new("api"), To: 8080}}

		_, _, err := svc.Apply(t.Context(), spec)
		require.NoError(t, err)

		require.Len(t, claimed, 1)
		assert.Equal(t, "api", claimed[0].Name)
		assert.Equal(t, 20000, claimed[0].Host)
	})

	t.Run("looks a pinned port up on the protocol it is pinned to", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound)
		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
		ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)

		// A workload holding 5353/tcp does not hold 5353/udp, so asking about the
		// wrong space would report a port as taken that nothing has.
		ports.EXPECT().HolderOf(mock.Anything, 5353, string(api.UDP)).Return("", false, nil).Once()

		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				w.ID, w.Version = "id-one", 1
				return w, true, nil
			})

		svc := newTestService(t, d, repo, ports, nil)

		spec := containerSpec("example", "example/example:latest")
		spec.Ports = &[]api.PortMapping{{To: 53, From: new(5353), Protocol: new(api.UDP)}}

		_, _, err := svc.Apply(t.Context(), spec)
		require.NoError(t, err)
	})
}

func TestWorkloadService_Apply_NoPortsAvailable(t *testing.T) {
	t.Parallel()

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, "example").
		Return(database.Workload{}, database.ErrWorkloadNotFound)
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil)

	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	svc := service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Allocator: allocatorStub{err: port.ErrRangeExhausted},
	})

	// A workload has to actually want a host port for allocation to be reached.
	spec := containerSpec("example", "example/example:latest")
	spec.Ports = &[]api.PortMapping{{To: 8080}}

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

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()

	// A daemon that accepts the call and never answers is the case a timeout exists
	// for: the observation is bounded by a context, so it ends when that context does
	// rather than when the driver decides to reply.
	d.EXPECT().Observe(mock.Anything).RunAndReturn(func(ctx context.Context) ([]driver.Instance, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}).Once()

	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	svc := service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: d},
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
	assert.Equal(t, service.WorkloadStatePending, got.State)
}

func TestWorkloadService_List_Queries(t *testing.T) {
	t.Parallel()

	t.Run("passes parsed queries to the repository", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().List(mock.Anything, []database.Query{{Path: "$.labels.app", Value: "web"}}).
			Return([]database.Workload{storedWorkload("example")}, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.List(t.Context(), "$.labels.app=web")
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("keeps an equals sign in the value", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.List(t.Context(), "$.labels.app")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})

	t.Run("reports a path the repository cannot parse", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

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
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		marked := storedWorkload("example")
		marked.DeletedAt = time.Now().UTC()

		repo.EXPECT().MarkDeleting(mock.Anything, "example").Return(marked, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", State: driver.StateRunning},
		}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.Delete(t.Context(), "example", false)
		require.NoError(t, err)

		// The reconciler owns stopping the work, so the service records the intent
		// and reports the workload as on its way out. A running instance does not
		// make it read as running any more.
		assert.True(t, got.Deleting)
		assert.Equal(t, service.WorkloadStateTerminating, got.State)
	})

	t.Run("notifies the reconciler", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().MarkDeleting(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		var notified bool
		svc := newTestService(t, d, repo, ports, func() { notified = true })

		_, err := svc.Delete(t.Context(), "example", false)
		require.NoError(t, err)

		// Nothing happens until the reconciler runs, so it has to be woken.
		assert.True(t, notified)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().MarkDeleting(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Delete(t.Context(), "nope", false)
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})

	t.Run("refuses a workload another one references", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().ReferencedBy(mock.Anything, "postgres").Return([]string{"api"}, nil).Once()

		svc := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:    newTestLogger(t),
			Drivers:   map[string]service.Driver{docker.Name: d},
			Workloads: repo,
			Ports:     ports,
			Allocator: allocatorStub{},
		})

		// Applying a workload that references one which does not exist is refused, so
		// removing this without asking would leave a specification nobody could
		// re-apply.
		_, err := svc.Delete(t.Context(), "postgres", false)
		require.ErrorIs(t, err, service.ErrWorkloadInUse)
		assert.Contains(t, err.Error(), "api")

		repo.AssertNotCalled(t, "MarkDeleting")
	})

	t.Run("deletes a referenced workload when forced", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		addresses := NewMockWorkloadAddresses(t)

		consumer := containerSpec("api", "example/example:latest")
		consumer.Env = &map[string]string{"DSN": "${workload:postgres:pg}"}

		consumerSpec, err := json.Marshal(consumer)
		require.NoError(t, err)

		repo.EXPECT().ReferencedBy(mock.Anything, "postgres").Return([]string{"api"}, nil).Once()
		repo.EXPECT().MarkDeleting(mock.Anything, "postgres").Return(storedWorkload("postgres"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		// The consumer is rehashed on the way out, the way a deleted secret rehashes
		// what read it: what it was started against no longer describes what orca
		// holds.
		repo.EXPECT().Get(mock.Anything, "api").
			Return(database.Workload{ID: "api-id", Name: "api", Spec: consumerSpec, SpecHash: "hash-one"}, nil).Once()
		addresses.EXPECT().Address(mock.Anything, mock.Anything).
			Return("", fmt.Errorf("%w: postgres", service.ErrWorkloadNotFound)).Once()
		repo.EXPECT().ReferencedBy(mock.Anything, "api").Return(nil, nil).Maybe()

		rehashed := make(chan database.Workload, 1)
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				rehashed <- w

				return w, false, nil
			}).Once()

		_, err = newTestAddressAwareService(t, d, repo, ports, addresses).Delete(t.Context(), "postgres", true)
		require.NoError(t, err)

		// The reference resolves against nothing now, so the consumer stops recording
		// an address and reports what it is missing when it next starts.
		require.Len(t, rehashed, 1)
		assert.NotEqual(t, "hash-one", (<-rehashed).SpecHash)
	})
}

func TestWorkloadService_Stop(t *testing.T) {
	t.Parallel()

	t.Run("suspends the workload without touching the runtime", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		suspended := storedWorkload("example")
		suspended.SuspendedAt = time.Now().UTC()

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		repo.EXPECT().Suspend(mock.Anything, "example").Return(suspended, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", State: driver.StateRunning},
		}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.Stop(t.Context(), "example")
		require.NoError(t, err)

		// The reconciler owns stopping the work, so the service records the intent
		// and reports the workload as held down. A running instance does not make
		// it read as running any more.
		assert.True(t, got.Suspended)
		assert.Equal(t, service.WorkloadStateSuspended, got.State)
	})

	t.Run("notifies the reconciler", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		repo.EXPECT().Suspend(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		var notified bool
		svc := newTestService(t, d, repo, ports, func() { notified = true })

		_, err := svc.Stop(t.Context(), "example")
		require.NoError(t, err)

		// Nothing stops until the reconciler runs, so it has to be woken.
		assert.True(t, notified)
	})

	t.Run("refuses a workload that is being deleted", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		deleting := storedWorkload("example")
		deleting.DeletedAt = time.Now().UTC()

		// No Suspend is expected: there is nothing left to hold down.
		repo.EXPECT().Get(mock.Anything, "example").Return(deleting, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Stop(t.Context(), "example")
		assert.ErrorIs(t, err, service.ErrWorkloadDeleting)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Stop(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_Start(t *testing.T) {
	t.Parallel()

	t.Run("clears the suspension", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		suspended := storedWorkload("example")
		suspended.SuspendedAt = time.Now().UTC()

		repo.EXPECT().Get(mock.Anything, "example").Return(suspended, nil).Once()
		repo.EXPECT().Resume(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		got, err := svc.Start(t.Context(), "example")
		require.NoError(t, err)

		// Nothing has started yet, so the workload reads as pending: the mark is
		// cleared and the reconciler brings the instances back.
		assert.False(t, got.Suspended)
		assert.Equal(t, service.WorkloadStatePending, got.State)
	})

	t.Run("notifies the reconciler", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		repo.EXPECT().Resume(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Once()

		var notified bool
		svc := newTestService(t, d, repo, ports, func() { notified = true })

		_, err := svc.Start(t.Context(), "example")
		require.NoError(t, err)

		// Nothing starts until the reconciler runs, so it has to be woken.
		assert.True(t, notified)
	})

	t.Run("refuses a workload that is being deleted", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		deleting := storedWorkload("example")
		deleting.DeletedAt = time.Now().UTC()

		// No Resume is expected: the workload can never run again.
		repo.EXPECT().Get(mock.Anything, "example").Return(deleting, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Start(t.Context(), "example")
		assert.ErrorIs(t, err, service.ErrWorkloadDeleting)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Start(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_Restart(t *testing.T) {
	t.Parallel()

	t.Run("records the request and wakes the reconciler", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		rec := NewMockReconciler(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
			{ID: "container-one", Workload: "example", SpecHash: "hash", State: driver.StateRunning},
		}, nil).Once()

		// The service records the request and the reconciler replaces the
		// instances, so the runtime is never touched from here.
		rec.EXPECT().Restart("example").Return().Once()
		rec.EXPECT().Notify().Return().Once()
		rec.EXPECT().LastError(mock.Anything).Return("", time.Time{}, false).Maybe()

		ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

		svc := service.NewWorkloadService(service.WorkloadServiceConfig{
			Logger:     newTestLogger(t),
			Drivers:    map[string]service.Driver{docker.Name: d},
			Workloads:  repo,
			Ports:      ports,
			Allocator:  allocatorStub{},
			Reconciler: rec,
		})

		got, err := svc.Restart(t.Context(), "example")
		require.NoError(t, err)

		// The specification and version are untouched, so the workload reads
		// exactly as it did: a restart is not a change of desired state.
		assert.Equal(t, service.WorkloadStateRunning, got.State)
	})

	t.Run("refuses a suspended workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		suspended := storedWorkload("example")
		suspended.SuspendedAt = time.Now().UTC()

		// The mock reconciler is deliberately absent: recording a request for a
		// workload that is held down would leave it lying in wait for the resume.
		repo.EXPECT().Get(mock.Anything, "example").Return(suspended, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Restart(t.Context(), "example")
		assert.ErrorIs(t, err, service.ErrWorkloadSuspended)
	})

	t.Run("refuses a workload that is being deleted", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		deleting := storedWorkload("example")
		deleting.DeletedAt = time.Now().UTC()

		repo.EXPECT().Get(mock.Anything, "example").Return(deleting, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Restart(t.Context(), "example")
		assert.ErrorIs(t, err, service.ErrWorkloadDeleting)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		_, err := svc.Restart(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

func TestWorkloadService_Apply_RejectsATerminatingWorkload(t *testing.T) {
	t.Parallel()

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	deleting := storedWorkload("example")
	deleting.DeletedAt = time.Now().UTC()

	repo.EXPECT().Get(mock.Anything, "example").Return(deleting, nil).Once()

	svc := newTestService(t, d, repo, ports, nil)

	// Re-applying a workload mid-teardown would race the reconciler removing it,
	// and could leave the freshly applied instance being torn down instead.
	_, _, err := svc.Apply(t.Context(), containerSpec("example", "example/example:latest"))
	assert.ErrorIs(t, err, service.ErrWorkloadDeleting)
}

func TestWorkloadService_Apply_HashesSecretRevisions(t *testing.T) {
	t.Parallel()

	t.Run("hashes a workload reading no secret exactly as before", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		var hash string
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				hash = w.SpecHash

				return w, true, nil
			}).Once()

		_, _, err := newTestService(t, d, repo, ports, nil).
			Apply(t.Context(), containerSpec("example", "example/example:latest"))
		require.NoError(t, err)

		// Pinned to the literal, because this is the hash orca computed before secrets
		// existed. A change here replaces every running instance on upgrade, so it has
		// to be a decision rather than a side effect.
		assert.Equal(t, "485029cc492e6cb9a301bdde6d7632286d9613ebae081824118e64611dcdf60f", hash)
	})

	t.Run("moves the hash when a secret's revision moves", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "postgres://app:${secret:db-password}@localhost/app"})

		first := applyForHash(t, spec, map[string]string{"db-password": "rev-one"})
		second := applyForHash(t, spec, map[string]string{"db-password": "rev-two"})

		// The same specification against a different revision is a different hash, so
		// the reconciler replaces the instances holding the old value.
		assert.NotEqual(t, first, second)
	})

	t.Run("keeps the hash when the revision is unchanged", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:db-password}"})

		first := applyForHash(t, spec, map[string]string{"db-password": "rev-one"})
		second := applyForHash(t, spec, map[string]string{"db-password": "rev-one"})

		// Re-applying an unchanged manifest against an unchanged secret must not
		// restart anything.
		assert.Equal(t, first, second)
	})

	t.Run("hashes a workload reading a secret differently from one that does not", func(t *testing.T) {
		plain := containerSpec("example", "example/example:latest")
		plain.Env = new(map[string]string{"DSN": "${secret:db-password}"})

		withSecret := applyForHash(t, plain, map[string]string{"db-password": "rev-one"})

		assert.NotEqual(t, "485029cc492e6cb9a301bdde6d7632286d9613ebae081824118e64611dcdf60f", withSecret)
	})

	t.Run("stores the reference rather than the value", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:db-password}"})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"db-password"}).
			Return(map[string]string{"db-password": "rev-one"}, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w

				return w, true, nil
			}).Once()

		_, _, err := newTestSecretAwareService(t, d, repo, ports, secrets).Apply(t.Context(), spec)
		require.NoError(t, err)

		// The API echoes the stored specification back, so a resolved value here would
		// be readable by anything that can reach the server.
		assert.Contains(t, string(stored.Spec), "${secret:db-password}")
		assert.NotContains(t, string(stored.Spec), "rev-one")

		// The names are recorded so that rotating the secret can find this workload.
		assert.Equal(t, []string{"db-password"}, stored.Secrets)
	})

	t.Run("refuses a reference to a secret that does not exist", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:nope}"})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"nope"}).Return(nil, nil).Once()

		// The workload could never start, and the operator applying it is the one who
		// can correct the name.
		_, _, err := newTestSecretAwareService(t, d, repo, ports, secrets).Apply(t.Context(), spec)
		require.ErrorIs(t, err, service.ErrSecretNotFound)
		assert.Contains(t, err.Error(), "nope")
	})

	t.Run("refuses a malformed reference", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:unterminated"})

		_, _, err := newTestService(t, d, repo, ports, nil).Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrInvalidSpec)
	})
}

func TestWorkloadService_Apply_HashesVariableValues(t *testing.T) {
	t.Parallel()

	t.Run("hashes a workload reading a secret exactly as it did before variables", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "postgres://app:${secret:db-password}@localhost/app"})

		// Pinned to the literal, because this is the hash orca computed for this
		// workload before variables existed. Adding a field to what is hashed would
		// otherwise replace every running instance that reads a secret, so both fields
		// are omitted when empty and this is the test that holds them to it.
		assert.Equal(t,
			"75ec730e704fa8d23450ee2a8101b3ef5dbe273494684d4512a4c59da0710efb",
			applyForHashOf(t, spec, map[string]string{"db-password": "rev-one"}, nil))
	})

	t.Run("moves the hash when a variable's value changes", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		first := applyForHashOf(t, spec, nil, map[string]string{"log-level": "debug"})
		second := applyForHashOf(t, spec, nil, map[string]string{"log-level": "info"})

		// The value is what is hashed, so a change to it is a change to the hash and
		// the reconciler replaces the instances reading the old one.
		assert.NotEqual(t, first, second)
	})

	t.Run("keeps the hash when the value is unchanged", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		first := applyForHashOf(t, spec, nil, map[string]string{"log-level": "debug"})
		second := applyForHashOf(t, spec, nil, map[string]string{"log-level": "debug"})

		// Re-applying an unchanged manifest against an unchanged variable must not
		// restart anything. Hashing the value rather than a revision is what makes a
		// variable deleted and re-created with the same value read as unchanged too.
		assert.Equal(t, first, second)
	})

	t.Run("hashes a workload reading a variable differently from one that does not", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		withVariable := applyForHashOf(t, spec, nil, map[string]string{"log-level": "debug"})

		assert.NotEqual(t, "485029cc492e6cb9a301bdde6d7632286d9613ebae081824118e64611dcdf60f", withVariable)
	})

	t.Run("hashes the two kinds into different places", func(t *testing.T) {
		asSecret := containerSpec("example", "example/example:latest")
		asSecret.Env = new(map[string]string{"VALUE": "${secret:shared}"})

		asVariable := containerSpec("example", "example/example:latest")
		asVariable.Env = new(map[string]string{"VALUE": "${var:shared}"})

		// A name held by both kinds contributes to a different field of what is hashed,
		// so the two never collide even given the same name and the same text.
		first := applyForHashOf(t, asSecret, map[string]string{"shared": "same"}, nil)
		second := applyForHashOf(t, asVariable, nil, map[string]string{"shared": "same"})

		assert.NotEqual(t, first, second)
	})

	t.Run("stores the reference rather than the value", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets, variables := NewMockSecretRevisions(t), NewMockVariableValues(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		variables.EXPECT().Values(mock.Anything, []string{"log-level"}).
			Return(map[string]string{"log-level": "debug"}, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w

				return w, true, nil
			}).Once()

		_, _, err := newTestReferenceAwareService(t, d, repo, ports, secrets, variables).Apply(t.Context(), spec)
		require.NoError(t, err)

		// A variable is stored as written, as a secret is. The value is not a secret,
		// but resolving it into the stored specification would mean a change to the
		// variable no longer reaching the workload that reads it.
		assert.Contains(t, string(stored.Spec), "${var:log-level}")
		assert.NotContains(t, string(stored.Spec), "debug")

		// The names are recorded so that changing the variable can find this workload.
		assert.Equal(t, []string{"log-level"}, stored.Variables)
	})

	t.Run("records both kinds a workload reads", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets, variables := NewMockSecretRevisions(t), NewMockVariableValues(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "postgres://app:${secret:db-password}@${var:db-host}/app"})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"db-password"}).
			Return(map[string]string{"db-password": "rev-one"}, nil).Once()
		variables.EXPECT().Values(mock.Anything, []string{"db-host"}).
			Return(map[string]string{"db-host": "localhost"}, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w

				return w, true, nil
			}).Once()

		_, _, err := newTestReferenceAwareService(t, d, repo, ports, secrets, variables).Apply(t.Context(), spec)
		require.NoError(t, err)

		assert.Equal(t, []string{"db-password"}, stored.Secrets)
		assert.Equal(t, []string{"db-host"}, stored.Variables)
	})

	t.Run("refuses a reference to a variable that does not exist", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets, variables := NewMockSecretRevisions(t), NewMockVariableValues(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:nope}"})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		variables.EXPECT().Values(mock.Anything, []string{"nope"}).Return(nil, nil).Once()

		// The workload could never start, and the operator applying it is the one who
		// can correct the name.
		_, _, err := newTestReferenceAwareService(t, d, repo, ports, secrets, variables).Apply(t.Context(), spec)
		require.ErrorIs(t, err, service.ErrVariableNotFound)
		assert.Contains(t, err.Error(), "nope")
	})

	t.Run("refuses a variable on a server holding none", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		_, _, err := newTestService(t, d, repo, ports, nil).Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrVariableNotFound)
	})
}

func TestWorkloadService_Apply_HashesImageDigest(t *testing.T) {
	t.Parallel()

	t.Run("hashes a workload without a pull policy exactly as before", func(t *testing.T) {
		// Pinned to the literal from before the pull policy existed. The resolver
		// mock is strict, so this also proves no registry round-trip is made for a
		// workload that never asked for one.
		assert.Equal(t,
			"485029cc492e6cb9a301bdde6d7632286d9613ebae081824118e64611dcdf60f",
			applyForDigestHash(t, containerSpec("example", "example/example:latest"), nil))
	})

	t.Run("moves the hash when the digest moves", func(t *testing.T) {
		spec := pullAlwaysSpec("example", "example/example:latest")

		first := applyForDigestHash(t, spec, map[string]string{"example/example:latest": "sha256:one"})
		second := applyForDigestHash(t, spec, map[string]string{"example/example:latest": "sha256:two"})

		// The digest is what is hashed, so a rebuilt tag is a specification change
		// and the reconciler replaces the instance running the old content.
		assert.NotEqual(t, first, second)
	})

	t.Run("keeps the hash when the digest is unchanged", func(t *testing.T) {
		spec := pullAlwaysSpec("example", "example/example:latest")

		first := applyForDigestHash(t, spec, map[string]string{"example/example:latest": "sha256:one"})
		second := applyForDigestHash(t, spec, map[string]string{"example/example:latest": "sha256:one"})

		// Re-applying an unchanged manifest against an unchanged tag must not
		// restart anything.
		assert.Equal(t, first, second)
	})

	t.Run("stores the specification without the digest", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		images := NewMockImageResolver(t)

		spec := pullAlwaysSpec("example", "example/example:latest")

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		images.EXPECT().Digest(mock.Anything, "example/example:latest").
			Return("sha256:abc123", nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w

				return w, true, nil
			}).Once()

		_, _, err := newTestImageAwareService(t, d, repo, ports, images).Apply(t.Context(), spec)
		require.NoError(t, err)

		// The API echoes the stored specification back as what was submitted, and
		// the operator did not write a digest.
		assert.NotContains(t, string(stored.Spec), "sha256:abc123")
	})

	t.Run("fails the apply when the digest cannot be resolved", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		images := NewMockImageResolver(t)

		spec := pullAlwaysSpec("example", "example/example:latest")

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		images.EXPECT().Digest(mock.Anything, "example/example:latest").
			Return("", errors.New("registry unreachable")).Once()

		// A hash computed without the digest would claim the image is unchanged
		// when nothing checked, so the apply is refused instead.
		_, _, err := newTestImageAwareService(t, d, repo, ports, images).Apply(t.Context(), spec)
		assert.Error(t, err)
	})

	t.Run("refuses a pull-always workload on a server that cannot resolve digests", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		spec := pullAlwaysSpec("example", "example/example:latest")

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		_, _, err := newTestService(t, d, repo, ports, nil).Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrInvalidSpec)
	})
}

func TestWorkloadService_Rehash(t *testing.T) {
	t.Parallel()

	t.Run("moves the hash without touching the specification", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:db-password}"})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		row := database.Workload{ID: "workload-id", Name: "example", Runtime: string(api.Container), Spec: encoded, SpecHash: "stale"}
		held := []database.Port{{WorkloadID: row.ID, Container: 80, Host: 20001, Protocol: "tcp", Dynamic: true}}

		repo.EXPECT().Get(mock.Anything, "example").Return(row, nil).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"db-password"}).
			Return(map[string]string{"db-password": "rev-two"}, nil).Once()
		ports.EXPECT().List(mock.Anything, row.ID).Return(held, nil).Once()

		var stored database.Workload
		var claimed []database.Port
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, p ...database.Port) (database.Workload, bool, error) {
				stored, claimed = w, p

				return w, false, nil
			}).Once()

		changed, err := newTestSecretAwareService(t, d, repo, ports, secrets).Rehash(t.Context(), "example")
		require.NoError(t, err)
		assert.True(t, changed)

		// Only the hash moves. Rewriting the specification would re-resolve ports and
		// volumes for a change that has nothing to do with either.
		assert.Equal(t, encoded, stored.Spec)
		assert.NotEqual(t, "stale", stored.SpecHash)

		// The ports have to travel with the write or it would clear them.
		assert.Equal(t, held, claimed)
		assert.Equal(t, []string{"db-password"}, stored.Secrets)
	})

	t.Run("writes nothing when the hash is unchanged", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:db-password}"})

		revisions := map[string]string{"db-password": "rev-one"}
		hash := applyForHash(t, spec, revisions)

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "workload-id", Name: "example", Spec: encoded, SpecHash: hash}, nil).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"db-password"}).Return(revisions, nil).Once()

		// Nothing changed, so nothing is written and nothing is redeployed. The mock
		// asserts no Upsert, since it was never told to expect one.
		changed, err := newTestSecretAwareService(t, d, repo, ports, secrets).Rehash(t.Context(), "example")
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("moves the hash when the secret has gone", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"DSN": "${secret:db-password}"})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "workload-id", Name: "example", Spec: encoded, SpecHash: "stale"}, nil).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"db-password"}).Return(nil, nil).Once()
		ports.EXPECT().List(mock.Anything, "workload-id").Return(nil, nil).Once()
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				return w, false, nil
			}).Once()

		// A force-deleted secret still moves the hash, so the workload stops claiming
		// it is running against something orca holds.
		changed, err := newTestSecretAwareService(t, d, repo, ports, secrets).Rehash(t.Context(), "example")
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("moves the hash when a variable's value changes", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets, variables := NewMockSecretRevisions(t), NewMockVariableValues(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "workload-id", Name: "example", Spec: encoded, SpecHash: "stale"}, nil).Once()
		variables.EXPECT().Values(mock.Anything, []string{"log-level"}).
			Return(map[string]string{"log-level": "info"}, nil).Once()
		ports.EXPECT().List(mock.Anything, "workload-id").Return(nil, nil).Once()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w

				return w, false, nil
			}).Once()

		// One Rehash serves both kinds, because it recomputes against whatever the
		// workload references rather than against what it was told changed.
		changed, err := newTestReferenceAwareService(t, d, repo, ports, secrets, variables).
			Rehash(t.Context(), "example")
		require.NoError(t, err)
		assert.True(t, changed)

		// The specification is written back exactly as it was read. Only the hash moves.
		assert.JSONEq(t, string(encoded), string(stored.Spec))
		assert.Equal(t, []string{"log-level"}, stored.Variables)
	})

	t.Run("writes nothing when a variable's value is unchanged", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets, variables := NewMockSecretRevisions(t), NewMockVariableValues(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"LEVEL": "${var:log-level}"})

		// The hash the workload already holds for this value, so the rehash finds
		// nothing to do. The mock has no Upsert expectation, so a write would fail.
		hash := applyForHashOf(t, spec, nil, map[string]string{"log-level": "debug"})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "workload-id", Name: "example", Spec: encoded, SpecHash: hash}, nil).Once()
		variables.EXPECT().Values(mock.Anything, []string{"log-level"}).
			Return(map[string]string{"log-level": "debug"}, nil).Once()

		changed, err := newTestReferenceAwareService(t, d, repo, ports, secrets, variables).
			Rehash(t.Context(), "example")
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("moves the hash when a pull-always image is rebuilt", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		images := NewMockImageResolver(t)

		spec := pullAlwaysSpec("example", "example/example:latest")

		// The hash the workload holds for the tag's previous content.
		hash := applyForDigestHash(t, spec, map[string]string{"example/example:latest": "sha256:one"})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "workload-id", Name: "example", Spec: encoded, SpecHash: hash}, nil).Once()
		images.EXPECT().Digest(mock.Anything, "example/example:latest").
			Return("sha256:two", nil).Once()
		ports.EXPECT().List(mock.Anything, "workload-id").Return(nil, nil).Once()

		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				return w, false, nil
			}).Once()

		// The digest is resolved again rather than remembered, so a rehash is also
		// where a rebuilt tag is noticed.
		changed, err := newTestImageAwareService(t, d, repo, ports, images).Rehash(t.Context(), "example")
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("fails when a pull-always image's registry is unreachable", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		images := NewMockImageResolver(t)

		spec := pullAlwaysSpec("example", "example/example:latest")

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{ID: "workload-id", Name: "example", Spec: encoded, SpecHash: "stale"}, nil).Once()
		images.EXPECT().Digest(mock.Anything, "example/example:latest").
			Return("", errors.New("registry unreachable")).Once()

		// A hash computed without the digest would claim the image is unchanged when
		// nothing checked, so the rehash fails instead — which surfaces through
		// whatever secret or variable change asked for it.
		_, err = newTestImageAwareService(t, d, repo, ports, images).Rehash(t.Context(), "example")
		assert.Error(t, err)
	})

	t.Run("reports a workload that does not exist", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		_, err := newTestService(t, d, repo, ports, nil).Rehash(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})
}

// applyForHash applies spec against the given secret revisions and returns the hash
// the service stored, so that a test can compare two hashes without repeating the
// mock wiring.
func TestWorkloadService_Apply_HashesMountedValues(t *testing.T) {
	t.Parallel()

	// The literal hash of a workload mounting nothing, which is what every assertion
	// about a mounted value not reaching the hash is compared against.
	const plainHash = "485029cc492e6cb9a301bdde6d7632286d9613ebae081824118e64611dcdf60f"

	t.Run("moves the hash when a mounted secret's revision moves", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{Secret: new("tls-cert"), To: "/etc/tls/cert.pem"}})

		first := applyForHash(t, spec, map[string]string{"tls-cert": "rev-one"})
		second := applyForHash(t, spec, map[string]string{"tls-cert": "rev-two"})

		// A mount that named no signal asked to be replaced, which is what a moved hash
		// arranges.
		assert.NotEqual(t, first, second)
	})

	t.Run("moves the hash when a mounted variable's value moves", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{Var: new("app-config"), To: "/etc/app/config.json"}})

		first := applyForHashOf(t, spec, nil, map[string]string{"app-config": "first"})
		second := applyForHashOf(t, spec, nil, map[string]string{"app-config": "second"})

		assert.NotEqual(t, first, second)
	})

	t.Run("keeps the hash when a signalled secret's revision moves", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{
			Secret: new("tls-cert"),
			To:     "/etc/tls/cert.pem",
			Signal: new(api.SIGHUP),
		}})

		first := applyForHash(t, spec, map[string]string{"tls-cert": "rev-one"})
		second := applyForHash(t, spec, map[string]string{"tls-cert": "rev-two"})

		// The whole point of naming a signal. A hash that moved would have the
		// reconciler replace the instance, which is the one thing the signal asks not to
		// happen.
		assert.Equal(t, first, second)
	})

	t.Run("keeps the hash when a signalled variable's value moves", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{
			Var:    new("app-config"),
			To:     "/etc/app/config.json",
			Signal: new(api.SIGUSR1),
		}})

		first := applyForHashOf(t, spec, nil, map[string]string{"app-config": "first"})
		second := applyForHashOf(t, spec, nil, map[string]string{"app-config": "second"})

		assert.Equal(t, first, second)
	})

	t.Run("moves the hash when the value is also read from the environment", func(t *testing.T) {
		// An environment variable is fixed once a process has started, so a workload
		// reading the value there has to be replaced however its mount asked to be told.
		spec := containerSpec("example", "example/example:latest")
		spec.Env = new(map[string]string{"CERT": "${secret:tls-cert}"})
		spec.Volumes = new([]api.VolumeMount{{
			Secret: new("tls-cert"),
			To:     "/etc/tls/cert.pem",
			Signal: new(api.SIGHUP),
		}})

		first := applyForHash(t, spec, map[string]string{"tls-cert": "rev-one"})
		second := applyForHash(t, spec, map[string]string{"tls-cert": "rev-two"})

		assert.NotEqual(t, first, second)
	})

	t.Run("hashes a workload whose only reading is signalled exactly as one reading nothing", func(t *testing.T) {
		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{
			Secret: new("tls-cert"),
			To:     "/etc/tls/cert.pem",
			Signal: new(api.SIGHUP),
		}})

		hash := applyForHash(t, spec, map[string]string{"tls-cert": "rev-one"})

		// Not the same as plainHash — the mount is part of the specification, so it is
		// part of the encoding. What must not appear is the revision, which is what the
		// next assertion covers by proving the hash does not move with it.
		assert.NotEqual(t, plainHash, hash)
	})

	t.Run("stores the mount as written rather than the value", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{Secret: new("tls-cert"), To: "/etc/tls/cert.pem"}})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"tls-cert"}).
			Return(map[string]string{"tls-cert": "rev-one"}, nil).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		var stored database.Workload
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				stored = w

				return w, true, nil
			}).Once()

		_, _, err := newTestSecretAwareService(t, d, repo, ports, secrets).Apply(t.Context(), spec)
		require.NoError(t, err)

		// A mounted value is stored as the name it reads, and no path is resolved for it:
		// where the file is written changes with every version, so storing one would move
		// the hash for a reason the operator did not ask for.
		assert.Contains(t, string(stored.Spec), `"secret":"tls-cert"`)
		assert.NotContains(t, string(stored.Spec), "rev-one")
		assert.NotContains(t, string(stored.Spec), `"from"`)

		// Recorded so that rotating the secret can find this workload, whatever the
		// mount asked to be told.
		assert.Equal(t, []string{"tls-cert"}, stored.Secrets)
	})

	t.Run("refuses a mount of a secret that does not exist", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{Secret: new("nope"), To: "/etc/tls/cert.pem"}})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		secrets.EXPECT().Revisions(mock.Anything, []string{"nope"}).Return(nil, nil).Once()

		// The workload could never start, and the operator applying it is the one who can
		// correct the name.
		_, _, err := newTestSecretAwareService(t, d, repo, ports, secrets).Apply(t.Context(), spec)
		require.ErrorIs(t, err, service.ErrSecretNotFound)
		assert.Contains(t, err.Error(), "nope")
	})

	t.Run("refuses a mount naming nothing to mount", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{To: "/etc/tls/cert.pem"}})

		_, _, err := newTestService(t, d, repo, ports, nil).Apply(t.Context(), spec)
		assert.ErrorIs(t, err, service.ErrInvalidSpec)
	})

	t.Run("does not ask the volume locator about a mounted value", func(t *testing.T) {
		// A mounted value is not a volume, so a server holding no volumes at all still
		// runs a workload that mounts one.
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		secrets := NewMockSecretRevisions(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Volumes = new([]api.VolumeMount{{Secret: new("tls-cert"), To: "/etc/tls/cert.pem"}})

		repo.EXPECT().Get(mock.Anything, "example").
			Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
		secrets.EXPECT().Revisions(mock.Anything, mock.Anything).
			Return(map[string]string{"tls-cert": "rev-one"}, nil).Once()
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				return w, true, nil
			}).Once()
		d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

		_, _, err := newTestSecretAwareService(t, d, repo, ports, secrets).Apply(t.Context(), spec)
		require.NoError(t, err)
	})
}

func applyForHash(t *testing.T, spec api.WorkloadSpec, revisions map[string]string) string {
	t.Helper()

	return applyForHashOf(t, spec, revisions, nil)
}

// applyForHashOf applies spec against the given secret revisions and variable values
// and returns the hash the service stored, so that a test can compare two hashes
// without repeating the mock wiring.
func applyForHashOf(t *testing.T, spec api.WorkloadSpec, revisions, values map[string]string) string {
	t.Helper()

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
	secrets, variables := NewMockSecretRevisions(t), NewMockVariableValues(t)

	repo.EXPECT().Get(mock.Anything, spec.Name).
		Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
	secrets.EXPECT().Revisions(mock.Anything, mock.Anything).Return(revisions, nil).Maybe()
	variables.EXPECT().Values(mock.Anything, mock.Anything).Return(values, nil).Maybe()
	d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

	var hash string
	repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
			hash = w.SpecHash

			return w, true, nil
		}).Once()

	_, _, err := newTestReferenceAwareService(t, d, repo, ports, secrets, variables).Apply(t.Context(), spec)
	require.NoError(t, err)

	return hash
}

// applyForDigestHash applies spec against a resolver answering the given digests and
// returns the hash the service stored. A nil digests map builds a service with no
// resolver at all, for the workloads that never ask for one.
func applyForDigestHash(t *testing.T, spec api.WorkloadSpec, digests map[string]string) string {
	t.Helper()

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, spec.Name).
		Return(database.Workload{}, database.ErrWorkloadNotFound).Once()
	d.EXPECT().Observe(mock.Anything).Return(nil, nil).Maybe()

	var hash string
	repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
			hash = w.SpecHash

			return w, true, nil
		}).Once()

	var images *MockImageResolver
	if digests != nil {
		images = NewMockImageResolver(t)
		for ref, digest := range digests {
			images.EXPECT().Digest(mock.Anything, ref).Return(digest, nil).Once()
		}
	}

	_, _, err := newTestImageAwareService(t, d, repo, ports, images).Apply(t.Context(), spec)
	require.NoError(t, err)

	return hash
}

func TestWorkloadService_Reallocate(t *testing.T) {
	t.Parallel()

	t.Run("stores the ports it settled on alongside the workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Ports = new([]api.PortMapping{{To: 80, From: new(20005)}})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		row := database.Workload{ID: "workload-id", Name: "example", Runtime: string(api.Container), Spec: encoded, SpecHash: "hash-one"}
		held := []database.Port{{WorkloadID: row.ID, Container: 80, Host: 20005, Protocol: "tcp", Dynamic: true}}

		repo.EXPECT().Get(mock.Anything, "example").Return(row, nil).Once()
		ports.EXPECT().List(mock.Anything, row.ID).Return(held, nil).Once()
		ports.EXPECT().Allocated(mock.Anything).Return(map[string][]int{"tcp": {20005}}, nil).Once()

		// The write has to carry the allocation. Upsert replaces a workload's ports
		// with whatever it is handed, so one given none would clear the rows and
		// leave the stored specification naming a host port nothing holds.
		var claimed []database.Port
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, p ...database.Port) (database.Workload, bool, error) {
				claimed = p
				return w, false, nil
			}).Once()

		svc := newTestService(t, d, repo, ports, nil)

		changed, err := svc.Reallocate(t.Context(), "example")
		require.NoError(t, err)
		assert.True(t, changed)

		require.Len(t, claimed, 1)
		assert.Equal(t, row.ID, claimed[0].WorkloadID)
		assert.Equal(t, 80, claimed[0].Container)
		assert.True(t, claimed[0].Dynamic)
		assert.NotEqual(t, 20005, claimed[0].Host)
	})

	t.Run("rehashes the workloads referencing it", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)
		addresses := NewMockWorkloadAddresses(t)

		spec := containerSpec("example", "example/example:latest")
		spec.Ports = new([]api.PortMapping{{To: 80, From: new(20005)}})

		encoded, err := json.Marshal(spec)
		require.NoError(t, err)

		row := database.Workload{ID: "workload-id", Name: "example", Runtime: string(api.Container), Spec: encoded, SpecHash: "hash-one"}
		held := []database.Port{{WorkloadID: row.ID, Container: 80, Host: 20005, Protocol: "tcp", Dynamic: true}}

		repo.EXPECT().Get(mock.Anything, "example").Return(row, nil).Once()
		ports.EXPECT().List(mock.Anything, row.ID).Return(held, nil)
		ports.EXPECT().Allocated(mock.Anything).Return(map[string][]int{"tcp": {20005}}, nil).Once()
		repo.EXPECT().Upsert(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
				return w, false, nil
			}).Once()

		// A host port is reallocated by the reconciler rather than by anything going
		// through the service, so this is the one place a consumer's hash moves for a
		// reason nobody asked for. Miss it and the reference reads as automatic and
		// quietly is not.
		consumer := containerSpec("api", "example/example:latest")
		consumer.Env = &map[string]string{"DSN": "${workload:example:http}"}

		consumerSpec, err := json.Marshal(consumer)
		require.NoError(t, err)

		repo.EXPECT().ReferencedBy(mock.Anything, "example").Return([]string{"api"}, nil).Once()
		repo.EXPECT().Get(mock.Anything, "api").
			Return(database.Workload{ID: "api-id", Name: "api", Spec: consumerSpec, SpecHash: "hash-two"}, nil).Once()
		addresses.EXPECT().Address(mock.Anything, mock.Anything).Return("10.0.0.5:20100", nil).Once()
		repo.EXPECT().ReferencedBy(mock.Anything, "api").Return(nil, nil).Maybe()

		rehashed := make(chan database.Workload, 1)
		repo.EXPECT().Upsert(mock.Anything, mock.MatchedBy(func(w database.Workload) bool {
			return w.Name == "api"
		}), mock.Anything).RunAndReturn(func(_ context.Context, w database.Workload, _ ...database.Port) (database.Workload, bool, error) {
			rehashed <- w

			return w, false, nil
		}).Once()

		changed, err := newTestAddressAwareService(t, d, repo, ports, addresses).Reallocate(t.Context(), "example")
		require.NoError(t, err)
		assert.True(t, changed)

		require.Len(t, rehashed, 1)
		assert.NotEqual(t, "hash-two", (<-rehashed).SpecHash)
	})

	t.Run("leaves a workload holding only pinned ports alone", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		row := storedWorkload("example")
		row.ID = "workload-id"

		repo.EXPECT().Get(mock.Anything, "example").Return(row, nil).Once()
		ports.EXPECT().List(mock.Anything, row.ID).
			Return([]database.Port{{WorkloadID: row.ID, Container: 80, Host: 8080, Protocol: "tcp"}}, nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		// A pinned port was asked for explicitly, so moving it would override the
		// operator rather than revise a guess.
		changed, err := svc.Reallocate(t.Context(), "example")
		require.NoError(t, err)
		assert.False(t, changed)
	})
}

func TestWorkloadService_Logs(t *testing.T) {
	t.Parallel()

	t.Run("returns the driver's output", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
		d.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 20}).
			RunAndReturn(func(_ context.Context, out io.Writer, _ string, _ driver.LogOptions) error {
				_, err := out.Write([]byte("hello world\n"))
				return err
			}).Once()

		svc := newTestService(t, d, repo, ports, nil)

		var out strings.Builder
		require.NoError(t, svc.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 20}))
		assert.Equal(t, "hello world\n", out.String())
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "nope").Return(database.Workload{}, database.ErrWorkloadNotFound).Once()

		svc := newTestService(t, d, repo, ports, nil)

		err := svc.Logs(t.Context(), io.Discard, "nope", driver.LogOptions{Tail: 20})
		assert.ErrorIs(t, err, service.ErrWorkloadNotFound)
	})

	t.Run("passes the request for an earlier attempt to the driver", func(t *testing.T) {
		d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

		repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()

		// The service does not know which attempt is which — the driver holds them, so
		// the selection goes through untouched.
		d.EXPECT().Logs(mock.Anything, mock.Anything, "example", driver.LogOptions{Tail: 20, Previous: true}).
			Return(nil).Once()

		svc := newTestService(t, d, repo, ports, nil)

		require.NoError(t, svc.Logs(t.Context(), io.Discard, "example", driver.LogOptions{Tail: 20, Previous: true}))
	})
}

func TestWorkloadService_Get_LeavesOutARetainedInstance(t *testing.T) {
	t.Parallel()

	d, repo, ports := newMockDriver(t), NewMockWorkloadRepository(t), NewMockPortRepository(t)

	repo.EXPECT().Get(mock.Anything, "example").Return(storedWorkload("example"), nil).Once()
	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Once()

	// What a workload looks like just after a restart: the attempt now running, and the
	// failed one the driver keeps so that its output can still be read.
	d.EXPECT().Observe(mock.Anything).Return([]driver.Instance{
		{ID: "current", Workload: "example", State: driver.StateRunning},
		{ID: "retained", Workload: "example", State: driver.StateFailed, ExitCode: 1, Retained: true},
	}, nil).Once()

	svc := newTestService(t, d, repo, ports, nil)

	workload, err := svc.Get(t.Context(), "example")
	require.NoError(t, err)

	// Only the live instance is reported, and the workload reads as running. Counting
	// the retained one would report a healthy workload as failed on the strength of the
	// attempt before it, which is the opposite of what keeping the output is for.
	require.Len(t, workload.Instances, 1)
	assert.Equal(t, "current", workload.Instances[0].ID)
	assert.Equal(t, service.WorkloadStateRunning, workload.State)
}

// newTestService builds a service whose port repository answers the reads every path
// makes, so that a test only has to set up the behaviour it is actually about. A
// non-nil notify is called whenever the service wakes the reconciler.
func newTestService(t *testing.T, d *MockDriver, repo *MockWorkloadRepository, ports *MockPortRepository, notify func()) *service.WorkloadService {
	t.Helper()

	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().ListAll(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().HolderOf(mock.Anything, mock.Anything, mock.Anything).Return("", false, nil).Maybe()

	// Nothing references these workloads unless a test says otherwise, so nothing is
	// rehashed when their addresses move.
	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	config := service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Allocator: allocatorStub{},
	}

	if notify != nil {
		rec := NewMockReconciler(t)
		rec.EXPECT().Notify().Run(notify).Return().Maybe()
		rec.EXPECT().LastError(mock.Anything).Return("", time.Time{}, false).Maybe()

		config.Reconciler = rec
	}

	return service.NewWorkloadService(config)
}

// newTestImageAwareService builds a service that can resolve image digests, for the
// tests about what a pull-always image does to a workload's hash.
func newTestImageAwareService(
	t *testing.T,
	d *MockDriver,
	repo *MockWorkloadRepository,
	ports *MockPortRepository,
	images *MockImageResolver,
) *service.WorkloadService {
	t.Helper()

	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().ListAll(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().HolderOf(mock.Anything, mock.Anything, mock.Anything).Return("", false, nil).Maybe()

	// Nothing references these workloads unless a test says otherwise, so nothing is
	// rehashed when their addresses move.
	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	config := service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Allocator: allocatorStub{},
	}

	// Left nil rather than set to a typed nil, so that the service sees no resolver
	// at all and a test can exercise a server that has none.
	if images != nil {
		config.Images = images
	}

	return service.NewWorkloadService(config)
}

// newTestSecretAwareService builds a service that can read secret revisions, for the
// tests about what a referenced secret does to a workload's hash.
func newTestSecretAwareService(
	t *testing.T,
	d *MockDriver,
	repo *MockWorkloadRepository,
	ports *MockPortRepository,
	secrets *MockSecretRevisions,
) *service.WorkloadService {
	t.Helper()

	return newTestReferenceAwareService(t, d, repo, ports, secrets, nil)
}

func newTestReferenceAwareService(
	t *testing.T,
	d *MockDriver,
	repo *MockWorkloadRepository,
	ports *MockPortRepository,
	secrets *MockSecretRevisions,
	variables *MockVariableValues,
) *service.WorkloadService {
	t.Helper()

	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().ListAll(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().HolderOf(mock.Anything, mock.Anything, mock.Anything).Return("", false, nil).Maybe()

	// Nothing references these workloads unless a test says otherwise, so nothing is
	// rehashed when their addresses move.
	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	config := service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Secrets:   secrets,
		Allocator: allocatorStub{},
	}

	// Left nil rather than set to a typed nil, so that the service sees no variable
	// store at all and a test can exercise a server that holds none.
	if variables != nil {
		config.Variables = variables
	}

	return service.NewWorkloadService(config)
}

// newTestAddressAwareService builds a service that can resolve the address of a
// referenced workload, for the tests about what such a reference does to a hash.
func newTestAddressAwareService(
	t *testing.T,
	d *MockDriver,
	repo *MockWorkloadRepository,
	ports *MockPortRepository,
	addresses *MockWorkloadAddresses,
) *service.WorkloadService {
	t.Helper()

	ports.EXPECT().List(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().ListAll(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().Allocated(mock.Anything).Return(nil, nil).Maybe()
	ports.EXPECT().HolderOf(mock.Anything, mock.Anything, mock.Anything).Return("", false, nil).Maybe()

	// Nothing references these workloads unless a test says otherwise, so nothing is
	// rehashed when their addresses move.
	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	repo.EXPECT().ReferencedBy(mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	return service.NewWorkloadService(service.WorkloadServiceConfig{
		Logger:    newTestLogger(t),
		Drivers:   map[string]service.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Addresses: addresses,
		Allocator: allocatorStub{},
	})
}

// The allocatorStub type hands out ports from a fixed base, so a test can predict
// what a workload will be allocated without standing up the real allocator.
type allocatorStub struct {
	// err is returned instead of a port, so a test can exercise what the service
	// does when the range has nothing left.
	err error
}

func (a allocatorStub) Allocate(protocols []port.Protocol, taken map[port.Protocol][]int) (int, error) {
	if a.err != nil {
		return 0, a.err
	}

	var claimed int
	for _, protocol := range protocols {
		claimed += len(taken[protocol])
	}

	return 20000 + claimed, nil
}

func containerSpec(name, image string) api.WorkloadSpec {
	return api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Container: &api.ContainerSpec{Image: image},
	}
}

// pullAlwaysSpec builds a container workload whose pull policy asks for the image's
// digest to reach the hash.
func pullAlwaysSpec(name, image string) api.WorkloadSpec {
	spec := containerSpec(name, image)
	spec.Container.Pull = new(api.PullPolicyAlways)

	return spec
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

// newMockDriver returns a driver mock that already answers Name, which every consumer
// calls to report which runtime it is talking about.
func newMockDriver(t *testing.T) *MockDriver {
	t.Helper()

	d := NewMockDriver(t)
	d.EXPECT().Name().Return(docker.Name).Maybe()

	return d
}

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
		first.Ports = &[]api.PortMapping{{From: new(21000), To: 8080}}

		_, _, err := svc.Apply(t.Context(), first)
		require.NoError(t, err)

		// A dynamic port that collides is orca's to resolve, so it is allocated again.
		// A pinned one is the caller's decision, and quietly moving it would hand back
		// a workload reachable somewhere other than where they asked for.
		second := containerSpec("second", "example/example:latest")
		second.Ports = &[]api.PortMapping{{From: new(21000), To: 8080}}

		_, _, err = svc.Apply(t.Context(), second)
		assert.ErrorIs(t, err, service.ErrHostPortTaken)
	})

	t.Run("gives up rather than retrying forever", func(t *testing.T) {
		svc, ports := newConcurrentTestService(t)

		// Pinned, so that the port the allocator below hands out is one something
		// definitely holds. The real allocator picks at random across its range, which
		// would make whether these two collide a matter of luck.
		taken := containerSpec("taken", "example/example:latest")
		taken.Ports = &[]api.PortMapping{{From: new(20000), To: 8080}}

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

func (b blindPorts) Allocated(context.Context) (map[string][]int, error) {
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
