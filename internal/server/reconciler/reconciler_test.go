package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w docker.Workload) bool {
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
				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w docker.Workload) bool {
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
				row.Runtime = string(api.Script)
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
				row.Runtime = string(api.Script)

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
				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w docker.Workload) bool {
					return w.Name == "broken"
				})).Return("", errors.New("no such image"))

				d.EXPECT().Start(mock.Anything, mock.MatchedBy(func(w docker.Workload) bool {
					return w.Name == "healthy"
				})).Return("container-one", nil)
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)
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
				Driver:    d,
				Workloads: repo,
				Interval:  time.Hour,
			})

			// Run converges once at startup, so a single pass is exercised with no
			// reliance on the ticker.
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() { done <- r.Run(ctx) }()

			passes.wait(t, 1)

			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestReconciler_Run_PacesFailedStarts(t *testing.T) {
	t.Parallel()

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

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
		RunAndReturn(func(context.Context, docker.Workload) (string, error) {
			starts.inc()
			return "", errors.New("no such image")
		})

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Driver:    d,
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

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

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
		Driver:    d,
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

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Driver:    d,
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

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Driver:    d,
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

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

	repo.EXPECT().List(mock.Anything).Return(nil, nil)

	passes := newCounter()
	d.EXPECT().Observe(mock.Anything).Run(func(context.Context) { passes.inc() }).Return(nil, nil)

	events := make(chan driver.Event)
	d.EXPECT().Watch(mock.Anything).Return(events, nil).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Driver:    d,
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

	d, repo := NewMockDriver(t), NewMockWorkloadRepository(t)

	d.EXPECT().Watch(mock.Anything).Return(nil, errors.New("docker is down")).Once()

	r := reconciler.New(reconciler.Config{
		Logger:    newTestLogger(t),
		Driver:    d,
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

func (c *counter) wait(t *testing.T, passes int) {
	t.Helper()

	require.Eventually(t, func() bool {
		return c.get() >= passes
	}, time.Second, time.Millisecond)
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
