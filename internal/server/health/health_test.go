package health_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/dsb-labs/takt/internal/server/health"
)

func TestChecker_Run(t *testing.T) {
	t.Parallel()

	t.Run("reports a workload that answers as healthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/healthz", r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)

		checker := run(t)
		checker.Set("example", 0, check(server.Listener.Addr().String(), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusHealthy)
	})

	t.Run("reports a workload answering an error as unhealthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		checker := run(t)

		// A process that is listening but answering errors is exactly what a check
		// exists to catch: the runtime reports it running, and it cannot serve.
		checker.Set("example", 0, check(server.Listener.Addr().String(), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		result, ok := checker.Result("example", 0)
		require.True(t, ok)
		assert.Contains(t, result.Error, "500")
	})

	t.Run("reports a workload nothing is listening on as unhealthy", func(t *testing.T) {
		checker := run(t)

		// A port nothing holds, so the connection is refused rather than timing out.
		checker.Set("example", 0, check(freeAddress(t), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusUnhealthy)
	})

	t.Run("checks a connection when no path is given", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })

		checker := run(t)

		// A workload speaking something other than HTTP is checked by connecting,
		// which needs nothing of the protocol it speaks.
		checker.Set("example", 0, check(listener.Addr().String(), ""))

		awaitStatus(t, checker, "example", health.StatusHealthy)
	})

	t.Run("recovers when a workload starts answering again", func(t *testing.T) {
		// The handler runs on the server's own goroutine while the test flips this,
		// so it needs to be written and read atomically.
		var healthy atomic.Bool

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)

		checker := run(t)
		checker.Set("example", 0, check(server.Listener.Addr().String(), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		// A workload that recovers must not stay condemned by failures it has since
		// stopped producing.
		healthy.Store(true)
		awaitStatus(t, checker, "example", health.StatusHealthy)

		result, ok := checker.Result("example", 0)
		require.True(t, ok)
		assert.Zero(t, result.Failures)
		assert.Empty(t, result.Error)
	})
}

func TestChecker_Run_StartPeriod(t *testing.T) {
	t.Parallel()

	checker := run(t)

	spec := check(freeAddress(t), "/healthz")
	// Long enough that the check cannot exhaust its retries within it.
	spec.StartPeriod = time.Minute
	spec.Retries = 1

	checker.Set("example", 0, spec)

	// Failures inside the start period are expected rather than meaningful: a
	// workload that takes time to become ready would otherwise be replaced for
	// failing checks it was never going to pass yet.
	require.Eventually(t, func() bool {
		result, ok := checker.Result("example", 0)

		return ok && result.Failures > 0
	}, 5*time.Second, 50*time.Millisecond)

	result, ok := checker.Result("example", 0)
	require.True(t, ok)
	assert.Equal(t, health.StatusStarting, result.Status)
}

func TestChecker_Run_SlowProbeDoesNotDelayOthers(t *testing.T) {
	t.Parallel()

	// A workload that accepts the connection and never answers, so every probe
	// against it runs until its timeout. This is the case that matters: a workload
	// which has wedged is one the operator most wants the others still checked
	// around.
	hung, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hung.Close() })

	// Signals that the hung workload's probe has actually connected, so the workload
	// below is registered into a checker that is demonstrably mid-probe rather than
	// one that merely ought to be by now.
	connected := make(chan struct{}, 1)

	go func() {
		for {
			// The connection is deliberately neither read nor closed: closing it
			// would answer the probe, and the request has to hang for the test to
			// mean anything. It goes away with the listener at cleanup.
			if _, err := hung.Accept(); err != nil {
				return
			}

			select {
			case connected <- struct{}{}:
			default:
			}
		}
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	checker := run(t)

	// Registered first and left to get its probe in flight, so the workload below is
	// registered into a checker that is already busy.
	slow := check(hung.Addr().String(), "/healthz")
	slow.Interval = 3 * time.Second
	slow.Timeout = 3 * time.Second
	checker.Set("hung", 0, slow)

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("the hung workload was never probed")
	}

	// A probe that blocks the loop would make this workload wait out the hung one's
	// timeout before being checked at all, however short an interval it asked for.
	started := time.Now()
	checker.Set("example", 0, check(server.Listener.Addr().String(), "/healthz"))

	require.Eventuallyf(t, func() bool {
		result, ok := checker.Result("example", 0)

		return ok && result.Status == health.StatusHealthy
	}, 5*time.Second, 5*time.Millisecond, "workload never became healthy")

	// Generous next to the three seconds the hung probe runs for, and far below it:
	// the assertion is that the two are unrelated, not that checking is fast.
	assert.Less(t, time.Since(started), 500*time.Millisecond,
		"a workload waited on an unrelated hung probe before its first check")
}

func TestChecker_Run_DoesNotOverlapProbes(t *testing.T) {
	t.Parallel()

	// Counted rather than asserted per request: the point is how many probes are in
	// flight at once, which only the handler can see.
	var concurrent, worst atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inflight := concurrent.Add(1)
		defer concurrent.Add(-1)

		for {
			if seen := worst.Load(); inflight <= seen || worst.CompareAndSwap(seen, inflight) {
				break
			}
		}

		// Longer than the interval, so a checker that started a probe per tick
		// regardless would pile them up.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	checker := run(t)

	spec := check(server.Listener.Addr().String(), "/healthz")
	spec.Interval = 50 * time.Millisecond
	spec.Timeout = time.Second
	checker.Set("example", 0, spec)

	awaitStatus(t, checker, "example", health.StatusHealthy)

	// Not waiting for probes is what removed the barrier between workloads. A
	// workload must still not accumulate probes faster than it answers them.
	assert.Equal(t, int64(1), worst.Load(), "a workload was probed while its previous probe was still running")
}

func TestChecker_Set(t *testing.T) {
	t.Parallel()

	t.Run("keeps history when the check is unchanged", func(t *testing.T) {
		checker := run(t)
		spec := check(freeAddress(t), "/healthz")

		checker.Set("example", 0, spec)
		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		// Re-applying an unchanged manifest must not reset the start period, or a
		// broken workload would look like it had only just begun starting.
		checker.Set("example", 0, spec)

		result, ok := checker.Result("example", 0)
		require.True(t, ok)
		assert.Equal(t, health.StatusUnhealthy, result.Status)
	})

	t.Run("starts afresh when the check changes", func(t *testing.T) {
		checker := run(t)

		checker.Set("example", 0, check(freeAddress(t), "/healthz"))
		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		// Failures counted against the old check say nothing about a new one, so a
		// changed address begins with a clean slate.
		changed := check(freeAddress(t), "/readyz")
		changed.StartPeriod = time.Minute
		checker.Set("example", 0, changed)

		result, ok := checker.Result("example", 0)
		require.True(t, ok)
		assert.Equal(t, health.StatusStarting, result.Status)
		assert.Zero(t, result.Failures)
	})
}

func TestChecker_Set_CancelsTheProbeItReplaces(t *testing.T) {
	t.Parallel()

	// A workload that accepts and never answers, so a probe against it is still
	// running when the specification is replaced.
	hung, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hung.Close() })

	connections := make(chan net.Conn, 8)

	go func() {
		for {
			conn, err := hung.Accept()
			if err != nil {
				return
			}

			connections <- conn
		}
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	checker := run(t)

	slow := check(hung.Addr().String(), "/healthz")
	slow.Interval = time.Minute
	slow.Timeout = time.Minute
	checker.Set("example", 0, slow)

	// Wait for the probe to be genuinely in flight before replacing it.
	select {
	case conn := <-connections:
		t.Cleanup(func() { _ = conn.Close() })
	case <-time.After(5 * time.Second):
		t.Fatal("the workload was never probed")
	}

	// Replacing the check must not leave the old probe running for its whole minute:
	// it is checking an address this workload no longer has.
	checker.Set("example", 0, check(server.Listener.Addr().String(), "/healthz"))

	// The new check answers at once, which it could not do if it were waiting on the
	// probe it replaced.
	awaitStatus(t, checker, "example", health.StatusHealthy)

	// The abandoned probe must have stopped rather than run out its minute. Its
	// goroutine is the observable part: the connection it opened stays established
	// either way, since abandoning a request does not close the socket.
	assert.Eventually(t, func() bool {
		return !slices.ContainsFunc(goroutines(t), func(stack string) bool {
			return strings.Contains(stack, "health.(*Checker).probe")
		})
	}, 10*time.Second, 50*time.Millisecond,
		"a probe was left running against the specification it replaced")

	// And the cancelled probe must not be recorded against the check that replaced
	// it, which it says nothing about.
	result, ok := checker.Result("example", 0)
	require.True(t, ok)
	assert.Zero(t, result.Failures)
	assert.Empty(t, result.Error)
}

func TestChecker_Forget(t *testing.T) {
	t.Parallel()

	checker := run(t)
	checker.Set("example", 0, check(freeAddress(t), "/healthz"))

	_, ok := checker.Result("example", 0)
	require.True(t, ok)

	checker.Forget("example")

	// Results for work nothing runs any more would otherwise accumulate for the life
	// of the server.
	_, ok = checker.Result("example", 0)
	assert.False(t, ok)
}

func TestChecker_Metrics(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	checker := health.New(health.Config{MeterProvider: provider})

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())

	go func() { done <- checker.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	// A probe against an address nothing listens on is refused immediately, which
	// is the cheapest way to get an outcome on record.
	checker.Set("example", 0, check(freeAddress(t), ""))
	awaitStatus(t, checker, "example", health.StatusUnhealthy)

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &collected))

	var found bool
	for _, scope := range collected.ScopeMetrics {
		for _, recorded := range scope.Metrics {
			if recorded.Name != "takt.health.check.duration" {
				continue
			}

			histogram, ok := recorded.Data.(metricdata.Histogram[float64])
			require.True(t, ok)

			for _, point := range histogram.DataPoints {
				workload, _ := point.Attributes.Value("workload")
				outcome, _ := point.Attributes.Value("outcome")
				if workload.AsString() == "example" && outcome.AsString() == "error" {
					found = point.Count >= 1
				}
			}
		}
	}

	assert.True(t, found, "expected a recorded probe duration for the failing workload")
}

// run returns a Checker whose loop is running for the duration of the test.
func run(t *testing.T) *health.Checker {
	t.Helper()

	checker := health.New(health.Config{})

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())

	go func() { done <- checker.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	return checker
}

// check returns a check with intervals short enough that a test doesn't wait on them.
func check(address, path string) health.Check {
	return health.Check{
		Address:  address,
		HTTP:     path,
		Interval: 50 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
		Retries:  1,
	}
}

func awaitStatus(t *testing.T, checker *health.Checker, workload string, want health.Status) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		result, ok := checker.Result(workload, 0)

		return ok && result.Status == want
	}, 10*time.Second, 50*time.Millisecond, "workload %q never reached %q", workload, want)
}

// goroutines returns the current goroutine stacks, one per entry, so a test can
// assert on whether particular work is still running.
func goroutines(t *testing.T) []string {
	t.Helper()

	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)

	return strings.Split(string(buf[:n]), "\n\n")
}

// freeAddress returns an address nothing is listening on, so a check against it is
// refused rather than left to time out.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	return address
}
