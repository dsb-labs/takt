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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/driver/docker"
	"github.com/dsb-labs/takt/internal/server/health"
	"github.com/dsb-labs/takt/internal/server/mount"
	"github.com/dsb-labs/takt/internal/server/reconciler"
	"github.com/dsb-labs/takt/pkg/manifest"
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

				d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil)
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
				d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil)
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
			Name: "stops the work of a suspended workload",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.SuspendedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// Stopped rather than discarded: unlike a deletion the workload comes
				// back, and the instance the driver retains keeps its last output
				// readable while it is down.
				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil)
			},
		},
		{
			Name: "leaves a suspended workload with nothing running alone",
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.SuspendedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// Suspension holds the workload down, so no Start is expected however
				// many passes run while the mark is set.
			},
		},
		{
			Name: "leaves a suspended workload's ended instance alone",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateExited},
			},
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.SuspendedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// The ended instance remains the workload's current one in the
				// driver's eyes, so a Stop here would repeat on every pass forever.
				// It stays as it is, keeping the last output readable, and no Stop
				// or Start is expected at all.
			},
		},
		{
			Name: "waits for a suspended workload that is still terminating",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateTerminating},
			},
			SetupMocks: func(_ *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.SuspendedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// The stop from an earlier pass is still in flight, so stopping it
				// again would race the runtime finishing the job.
			},
		},
		{
			Name: "tears down a workload that is both suspended and deleted",
			Observed: []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			},
			SetupMocks: func(d *MockDriver, repo *MockWorkloadRepository) {
				row := storedWorkload("example", "hash-one")
				row.SuspendedAt = time.Now().UTC()
				row.DeletedAt = time.Now().UTC()

				repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

				// Deletion outranks suspension: a suspended workload that is deleted
				// must still be discarded, or it could never disappear.
				d.EXPECT().Discard(mock.Anything, mock.Anything, "example").Return(nil)
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

func TestReconciler_Run_RestartsOnRequest(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{
		storedWorkload("example", "hash-one"),
	}, nil)

	// The instance is current and running, so nothing but the request explains a
	// stop. Both calls are expected exactly once: the pass that acts on the
	// request consumes it, so the passes that follow leave the replacement alone.
	d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil).Once()
	d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w driver.Workload) bool {
		return w.Name == "example" && w.SpecHash == "hash-one"
	})).Return("container-two", nil).Once()

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateRunning},
			}, nil
		})

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
	})

	r.Restart("example")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	// A second pass over the same state must not act on the request again.
	r.Notify()
	passes.wait(t, 2)
	awaitPasses(t, r, 2)

	cancel()
	require.NoError(t, <-done)
}

func TestReconciler_Run_CoalescesDriverEvents(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

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

	awaitPasses(t, r, 1)

	// A burst of events is one piece of news. Each send is a synchronous handoff,
	// so the whole burst lands inside the coalesce window and has to produce one
	// pass rather than one each.
	for range 20 {
		events <- driver.Event{Workload: "example"}
	}

	awaitPasses(t, r, 2)
	assert.EqualValues(t, 2, r.Passes())

	cancel()
	require.NoError(t, <-done)
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
			checker.EXPECT().ForgetInstance("example", 0).Maybe()
			checker.EXPECT().Result("example", 0).Return(tc.Result, tc.Checked)

			if tc.ExpectRestart {
				d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil).Once()
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

	// The address is resolved from the host port takt allocated, and probed over
	// loopback so the check never leaves the host.
	registered := make(chan health.Check, 1)
	checker.EXPECT().Set("example", 0, mock.Anything).
		Run(func(_ string, _ int, check health.Check) {
			select {
			case registered <- check:
			default:
			}
		}).Return()

	checker.EXPECT().Result("example", 0).Return(health.Result{Status: health.StatusHealthy}, true)

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
	tcp := []database.Port{{WorkloadID: "workload-one", Container: 80, Host: 20080, Protocol: "tcp"}}

	named := []database.Port{
		{WorkloadID: "workload-one", Name: "http", Container: 80, Host: 20080, Protocol: "tcp"},
		{WorkloadID: "workload-one", Name: "metrics", Container: 9090, Host: 20090, Protocol: "tcp"},
	}

	tt := []struct {
		Name           string
		Bind           string
		CheckedPort    string
		Ports          []database.Port
		ExpectedProbed string
	}{
		{
			Name:           "probes the port the check names",
			CheckedPort:    "metrics",
			Ports:          named,
			ExpectedProbed: "127.0.0.1:20090",
		},
		{
			// The number is the other way of writing the same thing, so it has to
			// select the same port.
			Name:           "probes the port the check numbers",
			CheckedPort:    "9090",
			Ports:          named,
			ExpectedProbed: "127.0.0.1:20090",
		},
		{
			Name:           "probes the interface a workload is published on",
			Bind:           "10.0.0.5",
			Ports:          tcp,
			ExpectedProbed: "10.0.0.5:20080",
		},
		{
			// Every interface includes loopback, so the check stays on the host.
			Name:           "probes loopback when published on every interface",
			Bind:           "0.0.0.0",
			Ports:          tcp,
			ExpectedProbed: "127.0.0.1:20080",
		},
		{
			Name:           "probes loopback when told nothing",
			Bind:           "",
			Ports:          tcp,
			ExpectedProbed: "127.0.0.1:20080",
		},
		{
			// A connection to a UDP port succeeds whatever is behind it, so probing
			// the UDP side would report every workload as healthy.
			Name: "probes the tcp side of a workload publishing both",
			Bind: "",
			Ports: []database.Port{
				{WorkloadID: "workload-one", Container: 53, Host: 20053, Protocol: "udp"},
				{WorkloadID: "workload-one", Container: 80, Host: 20080, Protocol: "tcp"},
			},
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
			checked.Spec = specWithCheckedPort("example", tc.CheckedPort)

			repo.EXPECT().List(mock.Anything).Return([]database.Workload{checked}, nil)

			ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
				"workload-one": tc.Ports,
			}, nil)

			registered := make(chan health.Check, 1)
			checker.EXPECT().Set("example", 0, mock.Anything).
				Run(func(_ string, _ int, check health.Check) {
					select {
					case registered <- check:
					default:
					}
				}).Return()

			checker.EXPECT().Result("example", 0).Return(health.Result{Status: health.StatusHealthy}, true)

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

	checker.EXPECT().Set("example", 0, mock.Anything).Return()
	checker.EXPECT().Result("example", 0).
		Return(health.Result{Status: health.StatusUnhealthy, Failures: 2}, true)

	// The replacement must not inherit the departed container's verdict: it would be
	// condemned for failures it never produced, and denied the start period every
	// newly started workload is owed.
	forgotten := make(chan struct{}, 1)
	notify := func() {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}
	checker.EXPECT().Forget("example").Run(func(string) { notify() }).Return().Maybe()
	checker.EXPECT().ForgetInstance("example", 0).Run(func(string, int) { notify() }).Return().Maybe()

	d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil).Once()
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
	notify := func() {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}
	checker.EXPECT().Forget("example").Run(func(string) { notify() }).Return().Maybe()
	checker.EXPECT().ForgetInstance("example", 0).Run(func(string, int) { notify() }).Return().Maybe()

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

func TestReconciler_Run_ForgetsChecksOfASuspendedWorkload(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
	ports, checker := NewMockPortRepository(t), NewMockChecker(t)

	// A suspended workload is deliberately down. Probing it would accrue failures
	// against something that is exactly as down as it was asked to be, and report
	// it unhealthy.
	suspended := storedWorkload("example", "hash-one")
	suspended.ID = "workload-one"
	suspended.Spec = specWithHealth("example")
	suspended.SuspendedAt = time.Now()

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{suspended}, nil)

	ports.EXPECT().ListAll(mock.Anything).Return(map[string][]database.Port{
		"workload-one": {{WorkloadID: "workload-one", Container: 80, Host: 20080}},
	}, nil)

	forgotten := make(chan struct{}, 1)
	notify := func() {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}
	checker.EXPECT().Forget("example").Run(func(string) { notify() }).Return().Maybe()
	checker.EXPECT().ForgetInstance("example", 0).Run(func(string, int) { notify() }).Return().Maybe()

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
		Overlap     manifest.OverlapPolicy
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
			Overlap: manifest.OverlapReplace,
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
			Overlap: manifest.OverlapSkip,
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
				// An occurrence clears the whole workload where a between-run retry
				// clears the one instance, and which path a case takes is its
				// business — either counts as the stop it expects.
				d.EXPECT().Stop(mock.Anything, mock.Anything, "example").Return(nil).Maybe()
				d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil).Maybe()
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

func TestReconciler_Run_SuspendedScheduleMissesOccurrences(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	applied := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	// Several occurrences are due by the time the pass runs, and the workload is
	// suspended: none of them run, on any pass. They are missed rather than
	// accumulated, and the resume decides when the schedule counts again — it
	// moves updated_at, so the first run after a resume is the next natural
	// occurrence rather than the last one missed.
	row := storedWorkload("example", "hash-one")
	row.Spec = specWithSchedule("example", "0 2 * * *", "")
	row.UpdatedAt = applied
	row.SuspendedAt = applied.Add(time.Hour)

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
		Now:       func() time.Time { return applied.Add(5 * 24 * time.Hour) },
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	// The mock has no Start expectation, so an occurrence firing on any of these
	// passes fails the test.
	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	r.Notify()
	passes.wait(t, 2)
	awaitPasses(t, r, 2)

	cancel()
	require.NoError(t, <-done)
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
	d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil).Once()
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
		Policy        manifest.RestartPolicy
		State         driver.State
		ExitCode      int
		ExpectRestart bool
	}{
		{
			Name:          "always restarts a clean exit",
			Policy:        manifest.RestartAlways,
			State:         driver.StateExited,
			ExpectRestart: true,
		},
		{
			Name:          "always restarts a failure",
			Policy:        manifest.RestartAlways,
			State:         driver.StateFailed,
			ExitCode:      1,
			ExpectRestart: true,
		},
		{
			// The job did what it was asked to do, so running it again would repeat
			// work nobody asked to repeat.
			Name:          "on-failure leaves a clean exit alone",
			Policy:        manifest.RestartOnFailure,
			State:         driver.StateExited,
			ExpectRestart: false,
		},
		{
			Name:          "on-failure restarts a failure",
			Policy:        manifest.RestartOnFailure,
			State:         driver.StateFailed,
			ExitCode:      1,
			ExpectRestart: true,
		},
		{
			Name:          "never leaves a clean exit alone",
			Policy:        manifest.RestartNever,
			State:         driver.StateExited,
			ExpectRestart: false,
		},
		{
			// Retired without being called a success: the reconciler stops acting on
			// it, and the state it reports still says the workload failed.
			Name:          "never leaves a failure alone",
			Policy:        manifest.RestartNever,
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
				d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil)
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
	row.Spec = specWithRestart("example", manifest.RestartOnFailure)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)

	d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil)
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
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      "example",
		Restart:   &manifest.Restart{Policy: manifest.RestartOnFailure},
		Health:    &manifest.Health{HTTP: "/healthz"},
		Container: &manifest.Container{Image: "example/example:latest"},
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
	notify := func() {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}
	checker.EXPECT().Forget("example").Run(func(string) { notify() }).Return().Maybe()
	checker.EXPECT().ForgetInstance("example", 0).Run(func(string, int) { notify() }).Return().Maybe()

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
	d.EXPECT().StopInstance(mock.Anything, mock.Anything, "example", 0).Return(nil).Maybe()
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
	// been accounted for. Otherwise a burst would collapse into a single pass and
	// prove nothing about the window.
	for i := 2; i <= 4; i++ {
		r.Notify()
		passes.wait(t, i)
	}

	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 1, starts.get(), "a failing workload was retried inside its backoff window")
}

func TestReconciler_Run_RemembersWhyAStartFailed(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{
		storedWorkload("example", "hash-one"),
	}, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	d.EXPECT().Start(mock.Anything, mock.Anything).Return("", errors.New("no such image"))

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	// A named clock, so the time the error reports is asserted rather than bounded.
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Drivers:   map[string]reconciler.Driver{docker.Name: d},
		Workloads: repo,
		Interval:  time.Hour,
		Now:       func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- r.Run(ctx) }()

	passes.wait(t, 1)
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)

	message, at, ok := r.LastError("example")
	require.True(t, ok, "a failed start left no error to report")
	assert.Contains(t, message, "no such image")
	assert.Equal(t, now, at)

	_, _, ok = r.LastError("other")
	assert.False(t, ok, "a workload that never failed reported an error")
}

func TestReconciler_Run_ClearsTheErrorOnceSettled(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{
		storedWorkload("example", "hash-one"),
	}, nil)

	// The first pass fails to start the workload, and every later pass observes an
	// instance that has been up for longer than the settle period — which is what
	// converging means, and what must take the error with it.
	started := atomic.Bool{}

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			if !started.Load() {
				return nil, nil
			}

			return []driver.Instance{{
				ID:        "instance-one",
				Workload:  "example",
				SpecHash:  "hash-one",
				State:     driver.StateRunning,
				StartedAt: time.Now().Add(-time.Minute),
			}}, nil
		})

	d.EXPECT().Start(mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, driver.Workload) (string, error) {
			started.Store(true)
			return "", errors.New("no such image")
		}).Once()

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

	awaitPasses(t, r, 1)

	_, _, ok := r.LastError("example")
	require.True(t, ok, "a failed start left no error to report")

	// The backoff from the failed start would keep a later pass from starting again,
	// but the instance the driver now reports is already up: the pass settles it and
	// the error goes with it.
	r.Notify()
	awaitPasses(t, r, 2)

	cancel()
	require.NoError(t, <-done)

	_, _, ok = r.LastError("example")
	assert.False(t, ok, "a settled workload still reported the failure before it")
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

		secrets.EXPECT().Resolve(mock.Anything, map[string]string{"DSN": "${secret:db-password}"}, mock.Anything, mock.Anything).
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
		secrets.EXPECT().Resolve(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, map[string]string, string, int) (map[string]string, error) {
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
			Reallocate: func(context.Context, string, int) (bool, error) {
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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

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

	// The superseded version's plaintext has to go, and it has to go after the
	// replacement is running. Sweeping first would pull the files out from under the
	// instance being replaced if the start then failed.
	t.Run("reclaims superseded values once the replacement has started", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.Version = 4

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()
		mounts.EXPECT().Deliver(mock.Anything, "workload-id", 4, mock.Anything).Return(nil, nil)

		var order []string

		d.EXPECT().Start(mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, driver.Workload) (string, error) {
				order = append(order, "start")

				return "instance-one", nil
			})

		reclaimed := make(chan int, 1)
		mounts.EXPECT().Reclaim("workload-id", 4).RunAndReturn(func(_ string, keep int) error {
			order = append(order, "reclaim")

			select {
			case reclaimed <- keep:
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

		// The version kept is the one that just started, so everything older goes.
		assert.Equal(t, 4, <-reclaimed)

		cancel()
		require.NoError(t, <-done)

		require.GreaterOrEqual(t, len(order), 2)
		assert.Equal(t, []string{"start", "reclaim"}, order[:2], "the sweep ran before the replacement started")
	})

	// A sweep that fails leaves plaintext nothing reads, which is worth a warning and
	// is not worth failing a workload that started. The next start sweeps it, because
	// this removes everything but the current version rather than one named.
	t.Run("starts the workload even when the sweep fails", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()
		mounts.EXPECT().Deliver(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(errors.New("permission denied"))

		started := make(chan string, 1)
		d.EXPECT().Start(mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, driver.Workload) (string, error) {
				select {
				case started <- "instance-one":
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

		assert.Equal(t, "instance-one", <-started)

		cancel()
		require.NoError(t, <-done)
	})

	t.Run("does not abandon ports when a value cannot be delivered", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
		mounts := NewMockMounts(t)
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

		delivers := newCounter()
		mounts.EXPECT().Deliver(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(context.Context, string, int, manifest.Spec) ([]driver.Volume, error) {
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
			Reallocate: func(context.Context, string, int) (bool, error) {
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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

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

func TestReconciler_Run_RemovesMountedValuesWhenSuspended(t *testing.T) {
	t.Parallel()

	d, repo := newMockDriver(t), NewMockWorkloadRepository(t)
	mounts := NewMockMounts(t)
	// Swept after every start, including the ones that changed nothing.
	mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

	row := storedWorkload("example", "hash-one")
	row.ID = "workload-id"
	row.SuspendedAt = time.Now().UTC()

	repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
	d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
	mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

	// Nothing is running, so the values the workload mounted have no reader left.
	// This is what takes a mounted secret's plaintext off the disk while the
	// workload is held down.
	forgotten := make(chan struct{}, 1)
	mounts.EXPECT().Forget("workload-id").Run(func(string) {
		select {
		case forgotten <- struct{}{}:
		default:
		}
	}).Return(nil)

	// The stopped instance is still reported: it is the workload's most recent
	// attempt, kept so its output stays readable. It has no reader for the mounted
	// values, so it must not stop them being removed.
	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).
		RunAndReturn(func(context.Context) ([]driver.Instance, error) {
			passes.inc()

			return []driver.Instance{
				{ID: "container-one", Workload: "example", SpecHash: "hash-one", State: driver.StateExited},
			}, nil
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
	<-forgotten
	awaitPasses(t, r, 1)

	cancel()
	require.NoError(t, <-done)
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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.Spec = specWithSignalledMount("example")

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Observe(mock.Anything).Return(running, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		mounts.EXPECT().Refresh(mock.Anything, "example", "workload-id", 1, mock.Anything).
			Return([]mount.Refresh{{
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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

		row := storedWorkload("example", "hash-one")
		row.ID = "workload-id"
		row.Spec = specWithSignalledMount("example")

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{row}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()
		mounts.EXPECT().Prune(mock.Anything).Return(nil).Maybe()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(running, nil)

		refreshed := []mount.Refresh{
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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

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
		// Swept after every start, including the ones that changed nothing.
		mounts.EXPECT().Reclaim(mock.Anything, mock.Anything).Return(nil).Maybe()

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
		d.EXPECT().StopInstance(mock.Anything, mock.Anything, mock.Anything, 0).Return(nil)

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

func TestReconciler_Observations(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	t.Run("reports a zero observation before the first pass", func(t *testing.T) {
		r := reconciler.New(reconciler.Config{
			Logger:  newTestLogger(t),
			Drivers: map[string]reconciler.Driver{docker.Name: NewMockDriver(t)},
		})

		// A driver that has never been asked must still be reported, or a
		// reader could not tell "not observed yet" from "no such driver".
		observations := r.Observations()
		require.Contains(t, observations, docker.Name)
		assert.True(t, observations[docker.Name].At.IsZero())
		assert.Empty(t, observations[docker.Name].Error)
	})

	t.Run("records a driver that answered", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().List(mock.Anything).Return(nil, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

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
			Now:       func() time.Time { return now },
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)
		awaitPasses(t, r, 1)

		cancel()
		require.NoError(t, <-done)

		observation := r.Observations()[docker.Name]
		assert.True(t, observation.At.Equal(now))
		assert.Empty(t, observation.Error)
	})

	t.Run("records a driver that failed to answer", func(t *testing.T) {
		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

		// The pass abandons the observation on the failure, so the workloads
		// may or may not be listed first — either order is fine here.
		repo.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).
			RunAndReturn(func(context.Context) ([]driver.Instance, error) {
				passes.inc()

				return nil, errors.New("daemon gone")
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Interval:  time.Hour,
			Now:       func() time.Time { return now },
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)
		awaitPasses(t, r, 1)

		cancel()
		require.NoError(t, <-done)

		observation := r.Observations()[docker.Name]
		assert.True(t, observation.At.Equal(now))
		assert.Equal(t, "daemon gone", observation.Error)
	})
}

func TestReconciler_Metrics(t *testing.T) {
	t.Parallel()

	t.Run("counts a pass that could not observe", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

		passes := newCounter()
		d.EXPECT().Observe(mock.Anything).
			RunAndReturn(func(context.Context) ([]driver.Instance, error) {
				passes.inc()

				return nil, errors.New("daemon gone")
			})

		r := reconciler.New(reconciler.Config{
			Logger:    newTestLogger(t),
			Drivers:   map[string]reconciler.Driver{docker.Name: d},
			Workloads: repo,
			Interval:  time.Hour,

			MeterProvider: provider,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)
		awaitPasses(t, r, 1)

		cancel()
		require.NoError(t, <-done)

		recorded := metricByName(t, reader, "takt.reconcile.passes")

		sum, ok := recorded.Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, sum.DataPoints, 1)

		point := sum.DataPoints[0]
		outcome, _ := point.Attributes.Value("outcome")
		assert.Equal(t, "observe_failed", outcome.AsString())
		assert.EqualValues(t, 1, point.Value)
	})

	t.Run("gauges workloads by state", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

		d, repo := newMockDriver(t), NewMockWorkloadRepository(t)

		repo.EXPECT().List(mock.Anything).Return([]database.Workload{
			storedWorkload("example", "hash-one"),
		}, nil)
		d.EXPECT().Watch(mock.Anything).Return(make(chan driver.Event), nil).Once()

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
			Interval:  time.Hour,

			MeterProvider: provider,
		})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- r.Run(ctx) }()

		passes.wait(t, 1)
		awaitPasses(t, r, 1)

		cancel()
		require.NoError(t, <-done)

		recorded := metricByName(t, reader, "takt.workloads")

		gauge, ok := recorded.Data.(metricdata.Gauge[int64])
		require.True(t, ok)

		counts := make(map[string]int64, len(gauge.DataPoints))
		for _, point := range gauge.DataPoints {
			state, _ := point.Attributes.Value("state")
			counts[state.AsString()] = point.Value
		}

		// Every state is recorded, so a state nothing is in reads as zero
		// rather than being absent from the scrape.
		assert.Len(t, counts, 8)
		assert.EqualValues(t, 1, counts["running"])
		assert.EqualValues(t, 0, counts["pending"])
		assert.EqualValues(t, 0, counts["suspended"])
	})
}

// metricByName returns the named metric from everything the reader has collected,
// failing the test when it was never recorded.
func metricByName(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &collected))

	for _, scope := range collected.ScopeMetrics {
		for _, recorded := range scope.Metrics {
			if recorded.Name == name {
				return recorded
			}
		}
	}

	t.Fatalf("no metric named %s was recorded", name)

	return metricdata.Metrics{}
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
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Container: &manifest.Container{Image: "example/example:latest"},
		Volumes: []manifest.VolumeMount{{
			Secret: "tls-cert",
			To:     "/etc/tls/cert.pem",
			Signal: manifest.SignalHUP,
		}},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithHealth returns a stored specification declaring a check, so the reconciler
// has something to resolve into a probe.
func specWithHealth(name string) []byte {
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Container: &manifest.Container{Image: "example/example:latest"},
		Health:    &manifest.Health{HTTP: "/healthz"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithCheckedPort returns a stored specification whose check names one of the
// workload's ports, or names none when port is empty.
func specWithCheckedPort(name, port string) []byte {
	health := manifest.Health{HTTP: "/healthz"}
	if port != "" {
		health.Port = manifest.PortRef(port)
	}

	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Container: &manifest.Container{Image: "example/example:latest"},
		Health:    &health,
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithSchedule returns a stored specification declaring a schedule.
func specWithSchedule(name, expression string, overlap manifest.OverlapPolicy) []byte {
	schedule := manifest.Schedule{Cron: expression}
	if overlap != "" {
		schedule.Overlap = overlap
	}

	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Schedule:  &schedule,
		Container: &manifest.Container{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithAttempts returns a stored specification capping how many times a workload is
// restarted.
func specWithAttempts(name string, attempts int) []byte {
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Restart:   &manifest.Restart{Attempts: attempts},
		Container: &manifest.Container{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithRestart returns a stored specification declaring a restart policy.
func specWithRestart(name string, policy manifest.RestartPolicy) []byte {
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Restart:   &manifest.Restart{Policy: policy},
		Container: &manifest.Container{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

// specWithEnv returns a stored specification setting the given environment.
func specWithEnv(name string, env map[string]string) []byte {
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Env:       env,
		Container: &manifest.Container{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return spec
}

func storedWorkload(name, hash string) database.Workload {
	spec, err := json.Marshal(manifest.Spec{
		Version:   "v1",
		Name:      name,
		Container: &manifest.Container{Image: "example/example:latest"},
	})
	if err != nil {
		panic(err)
	}

	return database.Workload{
		Name:     name,
		Version:  1,
		Runtime:  string(manifest.RuntimeContainer),
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
