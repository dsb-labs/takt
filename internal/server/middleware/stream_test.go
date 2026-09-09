package middleware_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/middleware"
)

// TestStream_LetsAHandlerClearTheWriteDeadline covers what the outermost middleware
// exists for.
//
// A followed log read is open for as long as its workload runs, which is longer than
// the deadline the server sets for a request that answers and stops. Unlike a flush,
// there is no reaching past the wrappers for this: http.ResponseController stops at the
// first one that cannot unwrap, so the writer has to be kept aside before any of them
// take it.
//
// A real server rather than a recorder, since a recorder has no deadline to clear and
// would pass whether the middleware worked or not.
func TestStream_LetsAHandlerClearTheWriteDeadline(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cleared := make(chan error, 1)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := middleware.Connection(r.Context())
		if conn == nil {
			cleared <- errors.New("the connection's own writer never reached the handler")

			return
		}

		cleared <- http.NewResponseController(conn).SetWriteDeadline(time.Time{})
	})

	server := httptest.NewServer(middleware.Stream(middleware.Wrap(handler, logger, nil, nil)))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/api/v1/workloads/example/logs")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.NoError(t, <-cleared)
}
