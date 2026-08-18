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
