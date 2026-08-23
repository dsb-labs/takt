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
	"github.com/dsb-labs/orca/internal/server/service"
	"github.com/dsb-labs/orca/pkg/manifest"
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

				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil)
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
				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil)
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

				// The work is discarded rather than stopped, and the row is left for a
				// later pass: the removal may not have taken effect yet, and removing
				// the desired state now would leave nothing describing work still
				// running.
				//
				// Discarded because keeping the instance for its output would mean the
				// driver never reported the workload gone, so the pass that removes the
				// row would never be reached.
				d.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(nil)
			},
		},
		{
			Name: "removes the desired state once a deleted workload has no work left",
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// Anything the driver kept for its output goes with the workload. The
				// output exists so an operator can read why an attempt failed, and a
				// deleted workload has no such reader.
				d.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(nil)

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

				// A failed teardown must not remove the row, or the container would be
				// left running with nothing recording that it exists.
				d.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(errors.New("docker is down"))
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

				// Discarded rather than stopped: nothing asked for this work, so there
				// is nobody to read its output, and an instance kept for that reason
				// would be found again on every pass from here on.
				d.EXPECT().Discard(mock.Anything, mock.Anything, "orphan").Return(nil)
			},
		},
		{
			Name: "reaps the retained instance of a workload nothing asked for",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "orphan", SpecHash: "hash-one", State: driver.StateExited, Retained: true},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return(nil, nil)

				// What a delete performed while the server was down leaves behind. A
				// retained instance is deliberately left out of what a pass converges,
				// so this is the case that proves it is not left out of the sweep as
				// well — one hidden from both would never be removed at all.
				d.EXPECT().Discard(mock.Anything, mock.Anything, "orphan").Return(nil)
			},
		},
		{
			Name: "leaves a workload whose only other instance is retained alone",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
				{ID: "container-two", Workload: "example", SpecHash: "hash-zero", State: driver.StateFailed, Retained: true},
			},
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Workload{storedWorkload("example", "hash-one")}, nil)

				// The retained instance holds an older specification and has failed, so
				// counting it would have the pass replace a workload that is running
				// perfectly well, and pace it as though it were crashing. Neither a stop
				// nor a start is expected.
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
				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil).Once()
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

func TestReconciler_Run_ProbesThePublishedAddress(t *testing.T) {
	t.Parallel()

	// A port published on one interface is only reachable there, so a server told to
	// publish somewhere specific has to be checked there too. Probing loopback
	// regardless would report every checked workload as unhealthy and have the
	// reconciler restart work that was answering perfectly well.
	tt := []struct {
		Name           string
		Bind           string
		ExpectedProbed string
	}{
		{
			Name:           "probes the interface a workload is published on",
			Bind:           "10.0.0.5",
			ExpectedProbed: "10.0.0.5:20080",
		},
		{
			// Every interface includes loopback, so the check stays on the host.
			Name:           "probes loopback when published on every interface",
			Bind:           "0.0.0.0",
			ExpectedProbed: "127.0.0.1:20080",
		},
		{
			Name:           "probes loopback when told nothing",
			Bind:           "",
			ExpectedProbed: "127.0.0.1:20080",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			repo := NewMockWorkloadRepository(t)
			ports := NewMockPortRepository(t)
			checker := NewMockChecker(t)
			d := NewMockDriver(t)

			checked := storedWorkload("example", "hash-one")
			checked.ID = "workload-one"
			checked.Spec = specWithHealth("example")

			repo.EXPECT().List(mock.Anything).Return([]database.Workload{checked}, nil)

			ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
				"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
			}, nil)

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
				Bind:      tc.Bind,
				Interval:  time.Hour,
			})

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() { done <- r.Run(ctx) }()

			passes.wait(t, 1)
			awaitPasses(t, r, 1)

			cancel()
			require.NoError(t, <-done)

			assert.Equal(t, tc.ExpectedProbed, (<-registered).Address)
		})
	}
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

	d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil).Once()
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

	d.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(nil)

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

	// The check being forgotten happens during the pass, so the row being removed may
	// still be in flight. Waiting for the pass to finish is what makes that expectation
	// deterministic.
	awaitPasses(t, r, 1)

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

	container.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(nil)

	// No Stop and no Discard is expected on the other driver at all, which is the
	// assertion: the mock fails the test if one arrives.
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
	container.EXPECT().Discard(mock.Anything, mock.Anything, "orphan").Return(nil)
	other.EXPECT().Discard(mock.Anything, mock.Anything, "orphan").Return(nil)

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
			// The first occurrence after the specification was applied. Nothing has
			// run, so there is no previous run to count from.
			Name:        "the first occurrence is due and nothing has ever run",
			Cron:        daily,
			Now:         ran.Add(24 * time.Hour),
			ExpectStart: true,
			ExpectStop:  true,
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
			// A workload that has not run counts its first occurrence from when its
			// specification was applied, so the fixture has to say when that was.
			row.UpdatedAt = ran

			repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

			if tc.ExpectStop {
				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil)
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
	d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil).Once()
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
				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil)
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

	d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil)
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

	d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil).Maybe()
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

func TestReconciler_Run_ResolvesSecrets(t *testing.T) {
	t.Parallel()

	t.Run("hands the driver the resolved environment", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		secrets := NewMockResolver(t)

		row := storedWorkload("example", "hash-one")
		row.Spec = specWithEnv("example", map[string]string{"DSN": "${secret:db-password}"})

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

		secrets.EXPECT().Resolve(mock.Anything, map[string]string{"DSN": "${secret:db-password}"}).
			Return(map[string]string{"DSN": "hunter2"}, nil)

		started := make(chan map[string]string, 1)
		d.EXPECT().Start(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w driver.Workload) (string, error) {
				select {
				case started <- w.Env:
				default:
				}

				return "instance-one", nil
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Env:       secrets,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		// The value reaches the driver, which is the only proof resolution happens on
		// the path that actually starts work.
		env := <-started

		cancel()
		require.NoError(t, <-done)

		assert.Equal(t, map[string]string{"DSN": "hunter2"}, env)
	})

	t.Run("does not abandon ports when a secret cannot be resolved", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		secrets := NewMockResolver(t)

		row := storedWorkload("example", "hash-one")
		row.Spec = specWithEnv("example", map[string]string{"DSN": "${secret:nope}"})

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

		resolves := newCounter()
		secrets.EXPECT().Resolve(mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, map[string]string) (map[string]string, error) {
				resolves.inc()

				return nil, errors.New("secret not found: nope")
			})

		// Reallocating would move the workload's address because a secret is missing,
		// which is a change an operator cannot account for. The mock has no
		// expectation for Start either, so reaching the driver at all would fail.
		reallocated := newCounter()

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Env:       secrets,
			Reallocate: func(context.Context, string) (bool, error) {
				reallocated.inc()

				return false, nil
			},
			Interval: time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)

		// Nudged twice more to show the backoff paces the retries, exactly as it does
		// for a workload whose image does not exist.
		for i := 2; i <= 3; i++ {
			r.Notify()
			passes.wait(t, i)
		}

		cancel()
		require.NoError(t, <-done)

		assert.Zero(t, reallocated.get(), "a missing secret gave up the workload's ports")
		assert.Equal(t, 1, resolves.get(), "a workload waiting on a secret was retried inside its backoff window")
	})

	t.Run("passes the environment through when nothing resolves secrets", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

		row := storedWorkload("example", "hash-one")
		row.Spec = specWithEnv("example", map[string]string{"PLAIN": "value"})

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

		started := make(chan map[string]string, 1)
		d.EXPECT().Start(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w driver.Workload) (string, error) {
				select {
				case started <- w.Env:
				default:
				}

				return "instance-one", nil
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

		env := <-started

		cancel()
		require.NoError(t, <-done)

		assert.Equal(t, map[string]string{"PLAIN": "value"}, env)
	})
}

func TestReconciler_Run_DeliversMountedValues(t *testing.T) {
	t.Parallel()

	t.Run("hands the driver the files it wrote", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		delivered := driver.Volume{
			Name:   "tls-cert",
			Host:   "/data/mounts/files/workload-id/1/secret-tls-cert",
			Target: "/etc/tls/cert.pem",
		}

		mounts.EXPECT().Deliver(mock.Anything, "workload-id", 1, mock.Anything).
			Return([]driver.Volume{delivered}, nil)

		started := make(chan []driver.Volume, 1)
		d.EXPECT().Start(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, w driver.Workload) (string, error) {
				select {
				case started <- w.Volumes:
				default:
				}

				return "instance-one", nil
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		// A materialised value reaches the driver as an ordinary mount, which is what
		// lets either runtime mount one without knowing where it came from.
		volumes := <-started

		cancel()
		require.NoError(t, <-done)

		assert.Equal(t, []driver.Volume{delivered}, volumes)
	})

	t.Run("does not abandon ports when a value cannot be delivered", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

		delivers := newCounter()
		mounts.EXPECT().Deliver(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, string, int, api.WorkloadSpec) ([]driver.Volume, error) {
				delivers.inc()

				return nil, errors.New("secret not found: nope")
			})

		// Ports have nothing to do with why this failed, exactly as for a secret an
		// environment reads. The mock has no expectation for Start either, so reaching
		// the driver at all would fail.
		reallocated := newCounter()

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Reallocate: func(context.Context, string) (bool, error) {
				reallocated.inc()

				return false, nil
			},
			Interval: time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)

		for i := 2; i <= 3; i++ {
			r.Notify()
			passes.wait(t, i)
		}

		cancel()
		require.NoError(t, <-done)

		assert.Zero(t, reallocated.get(), "a missing mounted value gave up the workload's ports")
		assert.Equal(t, 1, delivers.get(), "a workload waiting on a mounted value was retried inside its backoff window")
	})

	t.Run("removes what it wrote once the workload is gone", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.DeletedAt = time.Now()

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		d.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(nil)
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		forgotten := make(chan string, 1)
		mounts.EXPECT().Forget("workload-id").RunAndReturn(func(id string) error {
			select {
			case forgotten <- id:
			default:
			}

			return nil
		})

		repo.EXPECT().Delete(mock.Anything, "example").Return(nil)

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		// This is what takes a mounted secret's plaintext off the disk, and it has to
		// happen before the row goes: after that there is no identifier to find the
		// files by.
		id := <-forgotten

		cancel()
		require.NoError(t, <-done)

		assert.Equal(t, "workload-id", id)
	})

	t.Run("removes what it wrote for workloads that no longer exist", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).
			Return([]driver.Instance{{Workload: "example", SpecHash: "hash-one", State: driver.StateRunning}}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Refresh(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, nil).Maybe()

		pruned := make(chan []string, 1)
		mounts.EXPECT().Prune(mock.Anything).RunAndReturn(func(keep []string) error {
			select {
			case pruned <- keep:
			default:
			}

			return nil
		})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		// A teardown removes these itself. The prune is for the server that stopped
		// between stopping the work and removing the row.
		keep := <-pruned

		cancel()
		require.NoError(t, <-done)

		assert.Equal(t, []string{"workload-id"}, keep)
	})
}

func TestReconciler_Run_RefreshesMountedValues(t *testing.T) {
	t.Parallel()

	running := []driver.Instance{{
		Workload: "example",
		SpecHash: "hash-one",
		State:    driver.StateRunning,
	}}

	t.Run("signals a running workload whose mounted value changed", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.Spec = specWithSignalledMount("example")

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(running, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		mounts.EXPECT().Refresh(mock.Anything, "example", "workload-id", 1, mock.Anything).
			Return([]service.Refresh{{
				Reference: manifest.Reference{Kind: manifest.KindSecret, Name: "tls-cert"},
				Signal:    manifest.SignalHUP,
			}}, nil)

		signalled := make(chan string, 1)
		d.EXPECT().Signal(mock.Anything, "workload-id", "example", mock.Anything).
			RunAndReturn(func(_ context.Context, _, _, signal string) error {
				select {
				case signalled <- signal:
				default:
				}

				return nil
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		// The workload is told rather than replaced, which is what naming a signal asks
		// for. The mock has no expectation for Stop or Start, so replacing it would fail
		// the test.
		signal := <-signalled

		cancel()
		require.NoError(t, <-done)

		assert.Equal(t, "SIGHUP", signal)
	})

	t.Run("signals once for several mounts naming the same signal", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.Spec = specWithSignalledMount("example")

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(running, nil)

		refreshed := []service.Refresh{
			{
				Reference: manifest.Reference{Kind: manifest.KindSecret, Name: "tls-cert"},
				Signal:    manifest.SignalHUP,
			},
			{
				Reference: manifest.Reference{Kind: manifest.KindSecret, Name: "tls-key"},
				Signal:    manifest.SignalHUP,
			},
		}

		mounts.EXPECT().Refresh(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(refreshed, nil).Once()
		mounts.EXPECT().Refresh(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, nil).Maybe()

		signals := newCounter()
		d.EXPECT().Signal(mock.Anything, mock.Anything, mock.Anything, "SIGHUP").
			RunAndReturn(func(context.Context, string, string, string) error {
				signals.inc()

				return nil
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)
		awaitPasses(t, r, 1)

		cancel()
		require.NoError(t, <-done)

		// A workload that rotated three secrets wants to be told to reload, not told
		// three times.
		assert.Equal(t, 1, signals.get())
	})

	t.Run("does not signal a workload whose values are unchanged", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.Spec = specWithSignalledMount("example")

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(running, nil)

		// Nothing moved, so the workload is not disturbed. The mock has no expectation
		// for Signal, so sending one would fail the test.
		mounts.EXPECT().Refresh(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, nil)

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)

		// A second pass, so that an unchanged value is checked more than once and still
		// tells the workload nothing.
		r.Notify()
		passes.wait(t, 2)

		cancel()
		require.NoError(t, <-done)
	})

	t.Run("replaces a stale workload rather than refreshing it", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-two")
		row.ID = "workload-id"
		row.Spec = specWithSignalledMount("example")

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(running, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		// An instance about to be replaced has nothing worth signalling, and delivery
		// writes the current value as its replacement starts. The mock has no
		// expectation for Refresh or Signal, so either would fail the test.
		mounts.EXPECT().Deliver(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
		d.EXPECT().Stop(mock.Anything, mock.Anything, mock.Anything).Return(nil)

		started := make(chan struct{}, 1)
		d.EXPECT().Start(mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, driver.Workload) (string, error) {
				select {
				case started <- struct{}{}:
				default:
				}

				return "instance-two", nil
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Mounts:    mounts,
			Interval:  time.Hour,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		<-started

		cancel()
		require.NoError(t, <-done)
	})
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

// specWithSignalledMount returns a stored specification mounting a secret that asks to
// be signalled when it changes, which is what makes a workload worth refreshing.
func specWithSignalledMount(name string) []byte {
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Container: &api.ContainerSpec{Image: "example/example:latest"},
		Volumes: new([]api.VolumeMount{{
			Secret: new("tls-cert"),
			To:     "/etc/tls/cert.pem",
			Signal: new(api.SIGHUP),
		}}),
	})
	if err != nil {
		panic(err)
	}

	return spec
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

// specWithEnv returns a stored specification setting the given environment.
func specWithEnv(name string, env map[string]string) []byte {
	spec, err := json.Marshal(api.WorkloadSpec{
		Version:   "v1",
		Name:      name,
		Env:       new(env),
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
