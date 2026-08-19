package health_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/health"
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
		checker.Set("example", check(server.Listener.Addr().String(), "/healthz"))

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
		checker.Set("example", check(server.Listener.Addr().String(), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		result, ok := checker.Result("example")
		require.True(t, ok)
		assert.Contains(t, result.Error, "500")
	})

	t.Run("reports a workload nothing is listening on as unhealthy", func(t *testing.T) {
		checker := run(t)

		// A port nothing holds, so the connection is refused rather than timing out.
		checker.Set("example", check(freeAddress(t), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusUnhealthy)
	})

	t.Run("checks a connection when no path is given", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })

		checker := run(t)

		// A workload speaking something other than HTTP is checked by connecting,
		// which needs nothing of the protocol it speaks.
		checker.Set("example", check(listener.Addr().String(), ""))

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
		checker.Set("example", check(server.Listener.Addr().String(), "/healthz"))

		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		// A workload that recovers must not stay condemned by failures it has since
		// stopped producing.
		healthy.Store(true)
		awaitStatus(t, checker, "example", health.StatusHealthy)

		result, ok := checker.Result("example")
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

	checker.Set("example", spec)

	// Failures inside the start period are expected rather than meaningful: a
	// workload that takes time to become ready would otherwise be replaced for
	// failing checks it was never going to pass yet.
	require.Eventually(t, func() bool {
		result, ok := checker.Result("example")

		return ok && result.Failures > 0
	}, 5*time.Second, 50*time.Millisecond)

	result, ok := checker.Result("example")
	require.True(t, ok)
	assert.Equal(t, health.StatusStarting, result.Status)
}

func TestChecker_Set(t *testing.T) {
	t.Parallel()

	t.Run("keeps history when the check is unchanged", func(t *testing.T) {
		checker := run(t)
		spec := check(freeAddress(t), "/healthz")

		checker.Set("example", spec)
		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		// Re-applying an unchanged manifest must not reset the start period, or a
		// broken workload would look like it had only just begun starting.
		checker.Set("example", spec)

		result, ok := checker.Result("example")
		require.True(t, ok)
		assert.Equal(t, health.StatusUnhealthy, result.Status)
	})

	t.Run("starts afresh when the check changes", func(t *testing.T) {
		checker := run(t)

		checker.Set("example", check(freeAddress(t), "/healthz"))
		awaitStatus(t, checker, "example", health.StatusUnhealthy)

		// Failures counted against the old check say nothing about a new one, so a
		// changed address begins with a clean slate.
		changed := check(freeAddress(t), "/readyz")
		changed.StartPeriod = time.Minute
		checker.Set("example", changed)

		result, ok := checker.Result("example")
		require.True(t, ok)
		assert.Equal(t, health.StatusStarting, result.Status)
		assert.Zero(t, result.Failures)
	})
}

func TestChecker_Forget(t *testing.T) {
	t.Parallel()

	checker := run(t)
	checker.Set("example", check(freeAddress(t), "/healthz"))

	_, ok := checker.Result("example")
	require.True(t, ok)

	checker.Forget("example")

	// Results for work nothing runs any more would otherwise accumulate for the life
	// of the server.
	_, ok = checker.Result("example")
	assert.False(t, ok)
}

// run returns a Checker whose loop is running for the duration of the test.
func run(t *testing.T) *health.Checker {
	t.Helper()

	checker := health.New()

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
		result, ok := checker.Result(workload)

		return ok && result.Status == want
	}, 10*time.Second, 50*time.Millisecond, "workload %q never reached %q", workload, want)
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
