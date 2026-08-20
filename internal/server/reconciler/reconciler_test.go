package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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
	"github.com/dsb-labs/orca/internal/server/reconciler"
)

func TestReconciler_Run(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		Observed   []driver.Instance
		SetupMocks func(*MockDriver, *MockWorkloadRepository)
	}{
		{
			Name: "starts a workload that isn't running",
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("example", "hash-one"),
				}, nil)

				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
					return w.Name == "example" && w.SpecHash == "hash-one"
				})).Return("container-one", nil)
			},
		},
		{
			Name: "leaves a healthy workload alone",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("example", "hash-one"),
				}, nil)

				// The running instance is on the current spec hash, so there is
				// nothing to do — no Start or Stop is expected at all.
			},
		},
		{
			Name: "replaces an instance running a stale specification",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("example", "hash-two"),
				}, nil)

				d.EXPECT().Stop(mock.Anything, "example").Return(nil)
				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
					return w.SpecHash == "hash-two"
				})).Return("container-two", nil)
			},
		},
		{
			Name: "restarts an instance that exited",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateFailed, ExitCode: 1},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("example", "hash-one"),
				}, nil)

				// The corpse has to be cleared first: container names derive from
				// the workload and version, so a replacement would collide.
				d.EXPECT().Stop(mock.Anything, "example").Return(nil)
				d.EXPECT().Start(mock.Anything, mock.Anything).Return("container-two", nil)
			},
		},
		{
			Name: "waits for an instance that is still terminating",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateTerminating},
			},
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("example", "hash-one"),
				}, nil)

				// Teardown from an earlier pass is still in flight. Stopping what is
				// already stopping, or starting a replacement whose name the
				// departing container still holds, would both fail — so no Start or
				// Stop is expected at all.
			},
		},
		{
			Name: "waits rather than replacing an outdated instance that is terminating",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateTerminating},
			},
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				// The stored hash has moved on, so this instance is outdated as well
				// as terminating. It is already on its way out, so the replacement
				// waits for it to finish rather than racing its removal.
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("example", "hash-two"),
				}, nil)
			},
		},
		{
			Name: "stops the work of a workload marked for deletion",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// The work is stopped, but the row is left for a later pass: the
				// stop may not have taken effect yet, and removing the desired
				// state now would leave nothing describing work still running.
				d.EXPECT().Stop(mock.Anything, "example").Return(nil)
			},
		},
		{
			Name: "removes the desired state once a deleted workload has no work left",
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// The driver reports nothing for the workload, so there is nothing
				// left for the row to describe and it is finally removed.
				repo.EXPECT().Delete(mock.Anything, "example").Return(nil)
			},
		},
		{
			Name: "waits for a deleted workload that is still terminating",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateTerminating},
			},
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// Already on its way out, so neither stopping it again nor removing
				// the row is expected — the pass simply waits.
			},
		},
		{
			Name: "keeps the desired state when a deleted workload cannot be stopped",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// A failed stop must not remove the row, or the container would be
				// left running with nothing recording that it exists.
				d.EXPECT().Stop(mock.Anything, "example").Return(errors.New("docker is down"))
			},
		},
		{
			Name: "tears down a deleted workload whose runtime has no driver",
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.Runtime = "nothing-runs-this"
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// Nothing can be running for it, so deletion must still complete
				// rather than leaving an undeletable row behind.
				repo.EXPECT().Delete(mock.Anything, "example").Return(nil)
			},
		},
		{
			Name: "stops work nothing asked for",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "orphan", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return(nil, nil)

				d.EXPECT().Stop(mock.Anything, "orphan").Return(nil)
			},
		},
		{
			Name: "leaves a workload naming an unsupported runtime alone",
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.Runtime = "nothing-runs-this"

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
			},
		},
		{
			Name: "carries on when one workload fails to start",
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{
					storedWorkload("broken", "hash-one"),
					storedWorkload("healthy", "hash-one"),
				}, nil)

				// One broken workload must not stop the others converging.
				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
					return w.Name == "broken"
				})).Return("", errors.New("no such image"))

				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
					return w.Name == "healthy"
				})).Return("container-one", nil)
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
			tc.SetupMocks(d, repo)

			events := make(chan driver.Event)
			d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

			// Observe is called exactly once per pass, so completing it is the
			// signal that a pass has been driven — no polling of mock internals,
			// which would race with the loop's own calls.
			passes := newCounter()
			d.EXPECT().Observe(mock.Anything).
				RunAndReturn(func(context.Context) ([]driver.Instance, error) {
					passes.inc()
					return tc.Observed, nil
				})

			r := reconciler.New(reconciler.Config{
				Logger:    newTestLogger(t),
				Drivers:   map[string]reconciler.Driver{docker.Name: d},
				Workloads: repo,
				Interval:  time.Hour,
			})

			// Run converges once at startup, so a single pass is exercised with no
			// reliance on the ticker.
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() { done <- r.Run(ctx) }()

			passes.wait(t, 1)
			awaitPasses(t, r, 1)

			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestReconciler_Run_Health(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name          string
		Result        health.Result
		Checked       bool
		ExpectRestart bool
	}{
		{
			Name:    "leaves a workload passing its check alone",
			Result:  health.Result{Status: health.StatusHealthy},
			Checked: true,
		},
		{
			Name:    "leaves a workload still starting alone",
			Result:  health.Result{Status: health.StatusStarting},
			Checked: true,
			// A workload inside its start period has not failed anything — replacing
			// it would mean it never got the chance to become ready.
			ExpectRestart: false,
		},
		{
			Name:    "replaces a workload failing its check",
			Result:  health.Result{Status: health.StatusUnhealthy, Failures: 3},
			Checked: true,
			// The container is up as far as docker is concerned, and cannot serve.
			// Reacting to that is the entire reason for checking it.
			ExpectRestart: true,
		},
		{
			Name:    "leaves an unchecked workload alone",
			Checked: false,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
			ports, checker := NewMockPortRepository(t), NewMockChecker(t)

			row := storedWorkload("example", "hash-one")
			row.ID = "workload-one"

			repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

			ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
				"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
			}, nil)

			// The stored spec declares no check, so the reconciler has nothing to
			// register — the result is what decides the outcome here.
			checker.EXPECT().Forget("example").Maybe()
			checker.EXPECT().Result("example").Return(tc.Result, tc.Checked)

			if tc.ExpectRestart {
				d.EXPECT().Stop(mock.Anything, "example").Return(nil).Once()
				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
					return w.Name == "example"
				})).Return("container-two", nil).Once()
			}

			events := make(chan driver.Event)
			d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

			passes := newCounter()
			d.EXPECT().Observe(mock.Anything).
				RunAndReturn(func(context.Context) ([]driver.Instance, error) {
					passes.inc()

					return []driver.Instance{{
						ID:       "container-one",
						Workload: "example",
						SpecHash: "hash-one",
						State:    driver.StateRunning,
					}}, nil
				})

			r := reconciler.New(reconciler.Config{
				Logger:    newTestLogger(t),
				Drivers:   map[string]reconciler.Driver{docker.Name: d},
				Workloads: repo,
				Ports:     ports,
				Checker:   checker,
				Interval:  time.Hour,
			})

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() { done <- r.Run(ctx) }()

			passes.wait(t, 1)
			awaitPasses(t, r, 1)

			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestReconciler_Run_RegistersChecks(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
	ports, checker := NewMockPortRepository(t), NewMockChecker(t)

	checked := storedWorkload("example", "hash-one")
	checked.ID = "workload-one"
	checked.Spec = specWithHealth("example")

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{checked}, nil)

	ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
		"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
	}, nil)

	// The address is resolved from the host port orca allocated, and probed over
	// loopback so the check never leaves the host.
	registered := make(chan health.Check, 1)
	checker.EXPECT().Set("example", mock.Anything).
		Run(func(_ string, check health.Check) {
			select {
			case registered <- check:
			default:
			}
		}).Return()

	checker.EXPECT().Result("example").Return(health.Result{Status: health.StatusHealthy}, true)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateRunning,
			}}, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Checker:   checker,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)

	got := <-registered
	assert.Equal(t, "127.0.0.1:20080", got.Address)
	assert.Equal(t, "/healthz", got.HTTP)
}

func TestReconciler_Run_ForgetsChecksOnReplacement(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
	ports, checker := NewMockPortRepository(t), NewMockChecker(t)

	row := storedWorkload("example", "hash-one")
	row.ID = "workload-one"
	row.Spec = specWithHealth("example")

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

	ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
		"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
	}, nil)

	checker.EXPECT().Set("example", mock.Anything).Return()
	checker.EXPECT().Result("example").
		Return(health.Result{Status: health.StatusUnhealthy, Failures: 2}, true)

	// The replacement must not inherit the departed container's verdict: it would be
	// condemned for failures it never produced, and denied the start period every
	// newly started workload is owed.
	forgotten := make(chan struct{}, 1)
	checker.EXPECT().Forget("example").Run(func(string) {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}).Return()

	d.EXPECT().Stop(mock.Anything, "example").Return(nil).Once()
	d.EXPECT().Start(mock.Anything, mock.Anything).Return("container-two", nil).Once()

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateRunning,
			}}, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Checker:   checker,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	<-forgotten

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_ForgetsChecksOnTeardown(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
	ports, checker := NewMockPortRepository(t), NewMockChecker(t)

	// A workload on its way out is not worth probing, and results for one nothing
	// runs any more would otherwise accumulate for the life of the server.
	deleting := storedWorkload("example", "hash-one")
	deleting.ID = "workload-one"
	deleting.Spec = specWithHealth("example")
	deleting.DeletedAt = time.Now()

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{deleting}, nil)
	repo.EXPECT().Delete(mock.Anything, "example").Return(nil).Once()

	ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
		"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
	}, nil)

	forgotten := make(chan struct{}, 1)
	checker.EXPECT().Forget("example").Run(func(string) {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}).Return()

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()
			return nil, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Checker:   checker,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	<-forgotten

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_RoutesByRuntime(t *testing.T) {
	t.Parallel()

	container, other := newMockDriver(t), NewMockDriver(t)
	other.EXPECT().Name().Return("other").Maybe()

	repo := NewMockWorkloadRepository(t)

	// One workload for each runtime, so a driver that acted on the wrong one would be
	// caught by its own mock rather than by an assertion after the fact.
	mine := storedWorkload("mine", "hash-one")
	theirs := storedWorkload("theirs", "hash-one")
	theirs.Runtime = "other"

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{mine, theirs}, nil)

	container.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
		return w.Name == "mine"
	})).Return("container-one", nil)

	other.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
		return w.Name == "theirs"
	})).Return("other-one", nil)

	// Both drivers are observed and watched, because a pass has to see everything: a
	// workload it cannot see reads as absent and would be started again.
	events := make(chan driver.Event)
	container.EXPECT().Watch(mock.Anything).Return(events, nil).Once()
	other.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

	passes := newCounter()
	container.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()
			return nil, nil
		})
	other.EXPECT().Observe(mock.Anything).Return(nil, nil)

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: container, "other": other},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_ReadsTheInjectedClock(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{
		storedWorkload("example", "hash-one"),
	}, nil)

	// A clock the test controls, so a timing decision can be made to fall either way
	// without waiting for it. The backoff is what reads it.
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	var reads atomic.Int64

	// The workload cannot start, so the pass holds it and the next pass asks the clock
	// whether the hold has expired.
	d.EXPECT().Start(mock.Anything, mock.Anything).Return("", errors.New("cannot start"))

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()
			return nil, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  10 * time.Millisecond,
		Now: func() time.Time {
			reads.Add(1)

			return now
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 3)
	awaitPasses(t, r, 3)

	cancel()
	require.NoError(t, <-done)

	// The clock is the reconciler's only source of the time, so a pass that makes a
	// timing decision has to have read it.
	assert.Positive(t, reads.Load(), "the reconciler did not read the injected clock")
}

func TestReconciler_Run_ConvergesWorkloadsConcurrently(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	const workloads = 16

	rows := make([]database.Workload, 0, workloads)
	for i := range workloads {
		rows = append(rows, storedWorkload(fmt.Sprintf("w%02d", i), "hash-one"))
	}

	repo.EXPECT().List(mock.Anything).Return(rows, nil)

	// Every start blocks, which is what converging in turn made so expensive: most of
	// what a workload costs is waiting, so a serial pass adds those waits up.
	var inflight, worst atomic.Int64

	d.EXPECT().Start(mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, driver.Workload) (string, error) {
			concurrent := inflight.Add(1)
			defer inflight.Add(-1)

			for {
				if seen := worst.Load(); concurrent <= seen || worst.CompareAndSwap(seen, concurrent) {
					break
				}
			}

			time.Sleep(100 * time.Millisecond)

			return "id", nil
		})

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()
			return nil, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	started := time.Now()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)

	// Serially this would be sixteen hundred milliseconds. The bound is at least four,
	// so the pass has to be several times faster than that.
	assert.Less(t, time.Since(started), time.Second, "the pass converged workloads in turn")
	assert.Greater(t, worst.Load(), int64(1), "no two workloads were converged at once")
}

func TestReconciler_Run_StopsOnlyTheDriverThatRunsIt(t *testing.T) {
	t.Parallel()

	container, other := newMockDriver(t), NewMockDriver(t)
	other.EXPECT().Name().Return("other").Maybe()

	repo := NewMockWorkloadRepository(t)

	// A deleted workload belonging to one runtime. Asking the other to stop it costs a
	// round trip to something that was never going to have it, and a pass repeats that
	// for every workload — which was measured as the dominant cost of a teardown.
	row := storedWorkload("example", "hash-one")
	row.DeletedAt = time.Now().UTC()

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
	repo.EXPECT().Delete(mock.Anything, "example").Return(nil).Maybe()

	container.EXPECT().Stop(mock.Anything, "example").Return(nil)

	// No Stop is expected on the other driver at all, which is the assertion: the mock
	// fails the test if one arrives.
	events := make(chan driver.Event)
	container.EXPECT().Watch(mock.Anything).Return(events, nil).Once()
	other.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

	passes := newCounter()
	container.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateRunning,
			}}, nil
		})
	other.EXPECT().Observe(mock.Anything).Return(nil, nil)

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: container, "other": other},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_StopsAnOrphanOnEveryDriver(t *testing.T) {
	t.Parallel()

	container, other := newMockDriver(t), NewMockDriver(t)
	other.EXPECT().Name().Return("other").Maybe()

	repo := NewMockWorkloadRepository(t)
	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	// An orphan has no stored workload by definition, so there is no runtime to read
	// and no way to know which driver owns it. Both are asked, and a driver with
	// nothing for the name does nothing.
	container.EXPECT().Stop(mock.Anything, "orphan").Return(nil)
	other.EXPECT().Stop(mock.Anything, "orphan").Return(nil)

	events := make(chan driver.Event)
	container.EXPECT().Watch(mock.Anything).Return(events, nil).Once()
	other.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

	passes := newCounter()
	container.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "orphan",
				SpecHash: "hash-one",
				State:    driver.StateRunning,
			}}, nil
		})
	other.EXPECT().Observe(mock.Anything).Return(nil, nil)

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: container, "other": other},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_LeavesARuntimeWithNoDriverAlone(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	// A runtime nothing runs. The workload is stored and left as it is, so it starts
	// working when its driver arrives rather than being reported as broken.
	row := storedWorkload("example", "hash-one")
	row.Runtime = "nothing-runs-this"

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()
			return nil, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	// One pass is enough: the mocks expect no Start or Stop at all, so reaching the
	// end of a pass without either is the assertion.
	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_Schedule(t *testing.T) {
	t.Parallel()

	// A daily expression, so the times a test names are unambiguous.
	const daily = "0 2 * * *"

	ran := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	tt := []struct {
		Name        string
		Cron        string
		Overlap     api.OverlapPolicy
		Now         time.Time
		Instances   []driver.Instance
		ExpectStart bool
		ExpectStop  bool
	}{
		{
			// A schedule says when to run, and the moment a workload was applied is
			// not one of the times it names.
			Name:        "nothing has run and no occurrence is due",
			Cron:        daily,
			Now:         ran,
			ExpectStart: false,
		},
		{
			Name: "an occurrence is due and nothing is running",
			Cron: daily,
			Now:  ran.Add(24 * time.Hour),
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", SpecHash: "hash-one", State: driver.StateExited, StartedAt: ran},
			},
			ExpectStart: true,
			ExpectStop:  true,
		},
		{
			// The previous run is still going. Replace is what the schedule winning
			// means: the occurrence is honoured and the run is cut short.
			Name:    "an occurrence is due while a run is still going, replace",
			Cron:    daily,
			Overlap: api.Replace,
			Now:     ran.Add(24 * time.Hour),
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning, StartedAt: ran},
			},
			ExpectStart: true,
			ExpectStop:  true,
		},
		{
			// Skip is for a job that must not be interrupted. The occurrence is missed
			// rather than the run being cut short.
			Name:    "an occurrence is due while a run is still going, skip",
			Cron:    daily,
			Overlap: api.Skip,
			Now:     ran.Add(24 * time.Hour),
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning, StartedAt: ran},
			},
			ExpectStart: false,
			ExpectStop:  false,
		},
		{
			// Between occurrences a run that ended stays as it is, so its outcome is
			// readable rather than being replaced the moment it finishes.
			Name: "no occurrence is due and the last run ended",
			Cron: daily,
			Now:  ran.Add(time.Hour),
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", SpecHash: "hash-one", State: driver.StateExited, StartedAt: ran},
			},
			ExpectStart: false,
		},
		{
			// A failed run did not achieve what its occurrence asked for, so the
			// restart policy decides whether to try again before the next one is due.
			Name: "no occurrence is due and the last run failed",
			Cron: daily,
			Now:  ran.Add(time.Hour),
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", SpecHash: "hash-one", State: driver.StateFailed, ExitCode: 1, StartedAt: ran},
			},
			ExpectStart: true,
			ExpectStop:  true,
		},
		{
			// Several occurrences passed while the server was down. The next
			// occurrence after the last run is what is asked for, so the workload runs
			// once rather than once per occurrence missed.
			Name: "several occurrences were missed",
			Cron: daily,
			Now:  ran.Add(5 * 24 * time.Hour),
			Instances: []driver.Instance{
				{ID: "one", Workload: "example", SpecHash: "hash-one", State: driver.StateExited, StartedAt: ran},
			},
			ExpectStart: true,
			ExpectStop:  true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

			row := storedWorkload("example", "hash-one")
			row.Spec = specWithSchedule("example", tc.Cron, tc.Overlap)

			repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

			if tc.ExpectStop {
				d.EXPECT().Stop(mock.Anything, "example").Return(nil)
			}
			if tc.ExpectStart {
				d.EXPECT().Start(mock.Anything, mock.Anything).Return("two", nil)
			}

			events := make(chan driver.Event)
			d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

			passes := newCounter()
			d.EXPECT().Observe(mock.Anything).
				RunAndReturn(func(context.Context) ([]driver.Instance, error) {
					passes.inc()

					return tc.Instances, nil
				})

			r := reconciler.New(reconciler.Config{
				Logger:    newTestLogger(t),
				Drivers:   map[string]reconciler.Driver{docker.Name: d},
				Workloads: repo,
				Interval:  time.Hour,
				Now:       func() time.Time { return tc.Now },
			})

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() { done <- r.Run(ctx) }()

			passes.wait(t, 1)
			awaitPasses(t, r, 1)

			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestReconciler_Run_GivesUpAfterTheAttemptsAllowed(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	// One attempt allowed. The workload has already had it, so the pass leaves it as
	// it ended rather than trying again.
	row := storedWorkload("example", "hash-one")
	row.Spec = specWithAttempts("example", 1)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

	// The first pass restarts it, which uses the one attempt. Nothing after that.
	d.EXPECT().Stop(mock.Anything, "example").Return(nil).Once()
	d.EXPECT().Start(mock.Anything, mock.Anything).Return("two", nil).Once()

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateFailed,
				ExitCode: 1,
			}}, nil
		})

	// A clock that does not advance, so the backoff never expires on its own and the
	// attempts cap is the only thing that can stop the retries.
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  10 * time.Millisecond,
		Now:       func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 5)
	awaitPasses(t, r, 5)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_RestartPolicy(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name          string
		Policy        api.RestartPolicy
		State         driver.State
		ExitCode      int
		ExpectRestart bool
	}{
		{
			Name:          "always restarts a clean exit",
			Policy:        api.Always,
			State:         driver.StateExited,
			ExpectRestart: true,
		},
		{
			Name:          "always restarts a failure",
			Policy:        api.Always,
			State:         driver.StateFailed,
			ExitCode:      1,
			ExpectRestart: true,
		},
		{
			// The job did what it was asked to do, so running it again would repeat
			// work nobody asked to repeat.
			Name:          "on-failure leaves a clean exit alone",
			Policy:        api.OnFailure,
			State:         driver.StateExited,
			ExpectRestart: false,
		},
		{
			Name:          "on-failure restarts a failure",
			Policy:        api.OnFailure,
			State:         driver.StateFailed,
			ExitCode:      1,
			ExpectRestart: true,
		},
		{
			Name:          "never leaves a clean exit alone",
			Policy:        api.Never,
			State:         driver.StateExited,
			ExpectRestart: false,
		},
		{
			// Retired without being called a success: the reconciler stops acting on
			// it, and the state it reports still says the workload failed.
			Name:          "never leaves a failure alone",
			Policy:        api.Never,
			State:         driver.StateFailed,
			ExitCode:      1,
			ExpectRestart: false,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

			row := storedWorkload("example", "hash-one")
			row.Spec = specWithRestart("example", tc.Policy)

			repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

			if tc.ExpectRestart {
				// The corpse is cleared first, since container names derive from the
				// workload and version.
				d.EXPECT().Stop(mock.Anything, "example").Return(nil)
				d.EXPECT().Start(mock.Anything, mock.Anything).Return("container-two", nil)
			}

			events := make(chan driver.Event)
			d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

			passes := newCounter()
			d.EXPECT().Observe(mock.Anything).
				RunAndReturn(func(context.Context) ([]driver.Instance, error) {
					passes.inc()

					return []driver.Instance{{
						ID:       "container-one",
						Workload: "example",
						SpecHash: "hash-one",
						State:    tc.State,
						ExitCode: tc.ExitCode,
					}}, nil
				})

			r := reconciler.New(reconciler.Config{
				Logger:    newTestLogger(t),
				Drivers:   map[string]reconciler.Driver{docker.Name: d},
				Workloads: repo,
				Interval:  time.Hour,
			})

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() { done <- r.Run(ctx) }()

			passes.wait(t, 1)
			awaitPasses(t, r, 1)

			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestReconciler_Run_RerunsARetiredWorkloadWhenItsSpecChanges(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	// The instance that ran carries the old hash, and the stored specification has
	// moved on. That is what runs a finished job again: the operator changed what they
	// asked for, so what ran is out of date.
	row := storedWorkload("example", "hash-two")
	row.Spec = specWithRestart("example", api.OnFailure)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

	d.EXPECT().Stop(mock.Anything, "example").Return(nil)
	d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
		return w.SpecHash == "hash-two"
	})).Return("container-two", nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateExited,
			}}, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_ForgetsChecksOfARetiredWorkload(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
	ports, checker := NewMockPortRepository(t), NewMockChecker(t)

	row := storedWorkload("example", "hash-one")
	row.ID = "workload-one"
	// Declares both a check and a policy that retires it, which is the combination
	// that matters: the check has to stop when the workload finishes.
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      "example",
		Restart:   &api.RestartSpec{Policy: new(api.OnFailure)},
		Health:    &api.HealthSpec{HTTP: new("/healthz")},
		Container: &api.ContainerSpec{Image: "example/example:latest"},
	})
	require.NoError(t, err)
	row.Spec = spec

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

	ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
		"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
	}, nil)

	// A finished workload has nothing listening. Probing it would report it unhealthy
	// for no longer answering, which says nothing an operator can act on.
	forgotten := make(chan struct{}, 1)
	checker.EXPECT().Forget("example").Run(func(string) {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}).Return()

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateExited,
			}}, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Ports:     ports,
		Checker:   checker,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	<-forgotten

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_PacesAContainerThatExitsAtOnce(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{
		storedWorkload("example", "hash-one"),
	}, nil)

	// A container that exits the moment it starts is observed as running in the
	// instant between the two, so every pass alternates between seeing it up and
	// seeing it gone. Treating the former as convergence cleared the backoff each
	// time, and the workload was restarted as fast as the driver could be asked —
	// measured at five containers a second, indefinitely.
	var passes atomic.Int64

	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			// Alternating, and freshly started every time, which is what an
			// immediately-exiting container looks like from here.
			if passes.Add(1)%2 == 1 {
				return []driver.Instance{{
					ID:        "container-one",
					Workload:  "example",
					SpecHash:  "hash-one",
					State:     driver.StateRunning,
					StartedAt: time.Now(),
				}}, nil
			}

			return []driver.Instance{{
				ID:       "container-one",
				Workload: "example",
				SpecHash: "hash-one",
				State:    driver.StateExited,
			}}, nil
		})

	var starts atomic.Int64

	d.EXPECT().Stop(mock.Anything, "example").Return(nil).Maybe()
	d.EXPECT().Start(mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, driver.Workload) (string, error) {
			starts.Add(1)

			return "container-one", nil
		}).Maybe()

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		// Short enough that many passes run inside the window below, so an unpaced
		// workload has every opportunity to restart repeatedly.
		Interval: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	// Long enough for dozens of passes. The first restart is immediate and the
	// second waits out the base backoff, so a paced workload starts twice at most.
	require.Eventually(t, func() bool {
		return passes.Load() > 20
	}, 5*time.Second, 10*time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	assert.LessOrEqual(t, starts.Load(), int64(2),
		"a workload whose container exits at once was restarted on every pass")
}

func TestReconciler_Run_PacesFailedStarts(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{
		storedWorkload("example", "hash-one"),
	}, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	// A workload can fail to start for reasons retrying will never fix, such as an
	// image that does not exist. Without pacing it would be retried on every pass
	// and every event, achieving nothing but noise.
	starts := newCounter()
	d.EXPECT().Start(mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, driver.Workload) (string, error) {
			starts.inc()
			return "", errors.New("no such image")
		})

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	// The startup pass tries once and fails, which opens the backoff window.
	passes.wait(t, 1)

	// Nudges are coalesced, so each one is requested only after the previous pass has
	// been accounted for; otherwise a burst would collapse into a single pass and
	// prove nothing about the window.
	for i := 2; i <= 4; i++ {
		r.Notify()
		passes.wait(t, i)
	}

	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 1, starts.get(), "a failing workload was retried inside its backoff window")
}

func TestReconciler_Run_SurvivesAHangingDriver(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	// A pass is serial across workloads, so a daemon that accepts a call and never
	// answers would otherwise stop every workload converging behind it. The call has
	// to end when its context does.
	observed := newCounter()
	d.EXPECT().Observe(mock.Anything).RunAndReturn(func(ctx context.Context) ([]driver.Instance, error) {
		observed.inc()
		<-ctx.Done()

		return nil, ctx.Err()
	})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	// Cancelling the run's context is what the deadline ultimately relies on, so a
	// shutdown must not be blocked by a driver that is still refusing to answer.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	observed.wait(t, 1)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return while the driver was hanging")
	}
}

func TestReconciler_Run_ReconcilesOnDriverEvent(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		// Long enough that a pass can only be attributed to the event.
		Interval: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	// The startup pass has to be accounted for before the event is sent, so that
	// the second pass can only be attributed to the event itself.
	passes.wait(t, 1)

	// An event should drive a pass well before the ticker would have.
	events <- driver.Event{Workload: "example"}
	passes.wait(t, 2)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_ReconcilesOnNotify(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)

	// Notify is how the service reports that desired state changed, and must not
	// block even when a pass is already pending.
	r.Notify()
	r.Notify()

	passes.wait(t, 2)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_SurvivesEventStreamClosing(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	// A dead event stream costs responsiveness, not correctness: the ticker still
	// has to keep converging rather than the loop spinning or exiting.
	close(events)

	passes.wait(t, 3)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_FailsWhenTheDriverCannotBeWatched(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	d.EXPECT().Watch(mock.Anything).Return(nil, errors.New("docker is down")).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	// Failing to establish the watch means the loop can never be event-driven,
	// which is a startup failure the server should report rather than paper over.
	assert.Error(t, r.Run(t.Context()))
}

// The counter type counts reconciliation passes as the loop drives them, so that
// tests can wait on the pass itself rather than polling the mock's call log —
// which the loop is concurrently writing to.
type counter struct {
	mux   sync.Mutex
	count int
}

func newCounter() *counter {
	return &counter{}
}

func (c *counter) inc() {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.count++
}

func (c *counter) get() int {
	c.mux.Lock()
	defer c.mux.Unlock()

	return c.count
}

// wait blocks until at least the given number of passes have begun.
//
// Observing is the first thing a pass does, so this says a pass started rather than
// that it finished. Use awaitPasses to wait for the work of a pass to be done.
func (c *counter) wait(t *testing.T, passes int) {
	t.Helper()

	require.Eventually(t, func() bool {
		return c.get() >= passes
	}, 5*time.Second, time.Millisecond)
}

// awaitPasses blocks until the reconciler reports the given number of completed
// passes, which is what says the work of a pass has finished.
//
// Workloads are converged concurrently, so a mock expectation may still be in flight
// when a pass is observed to have started. The reconciler counts what it has finished,
// which is a signal rather than a guess.
func awaitPasses(t *testing.T, r *reconciler.Reconciler, passes uint64) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		return r.Passes() >= passes
	}, 5*time.Second, time.Millisecond, "the reconciler completed fewer than %d passes", passes)
}

// specWithHealth returns a stored specification declaring a check, so the reconciler
// has something to resolve into a probe.
func specWithHealth(name string) []byte {
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Container: &api.ContainerSpec{Image: "example/example:latest"},
		Health:    &api.HealthSpec{HTTP: new("/healthz")},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithSchedule returns a stored specification declaring a schedule.
func specWithSchedule(name, expression string, overlap api.OverlapPolicy) []byte {
	schedule := api.ScheduleSpec{Cron: expression}
	if overlap != "" {
		schedule.Overlap = new(overlap)
	}

	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Schedule:  &schedule,
		Container: &api.ContainerSpec{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithAttempts returns a stored specification capping how many times a workload is
// restarted.
func specWithAttempts(name string, attempts int) []byte {
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Restart:   &api.RestartSpec{Attempts: new(attempts)},
		Container: &api.ContainerSpec{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithRestart returns a stored specification declaring a restart policy.
func specWithRestart(name string, policy api.RestartPolicy) []byte {
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Restart:   &api.RestartSpec{Policy: new(policy)},
		Container: &api.ContainerSpec{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

func storedWorkload(name, hash string) database.Workload {
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Container: &api.ContainerSpec{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return database.Workload{
		Name:     name,
		Version:  1,
		Runtime:  string(api.Container),
		Spec:     spec,
		SpecHash: hash,
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
