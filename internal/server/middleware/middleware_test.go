package middleware_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/middleware"
)

func TestWrap(t *testing.T) {
	t.Parallel()

	// What the server sends a request to. Panicking is the interesting case, and the
	// rest of the chain has to carry a request to it for any of this to mean anything.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("something a handler did not expect")
		}

		w.WriteHeader(http.StatusOK)
	})

	tt := []struct {
		Name         string
		Method       string
		Path         string
		Host         string
		ContentType  string
		Body         string
		ExpectStatus int
	}{
		{
			Name: "answers a panicking handler with a server error",
			// The whole point of the chain rather than Recovery alone: a middleware
			// added above it, or the order changed, silently stops catching this.
			Method:       http.MethodGet,
			Path:         "/panic",
			Host:         "127.0.0.1:7373",
			ExpectStatus: http.StatusInternalServerError,
		},
		{
			Name:         "carries an ordinary request to the handler",
			Method:       http.MethodGet,
			Path:         "/api/v1/workloads",
			Host:         "127.0.0.1:7373",
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "refuses an unexpected host before reaching the handler",
			// Guard is inside Recovery but outside the handler, so a refusal here is a
			// request the handler never saw.
			Method:       http.MethodGet,
			Path:         "/panic",
			Host:         "takt.evil.example.com:7373",
			ExpectStatus: http.StatusMisdirectedRequest,
		},
		{
			Name:         "refuses a body that does not declare itself as json",
			Method:       http.MethodPut,
			Path:         "/api/v1/workloads/example",
			Host:         "127.0.0.1:7373",
			ContentType:  "text/plain",
			Body:         "{}",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "accepts a json body",
			Method:       http.MethodPut,
			Path:         "/api/v1/workloads/example",
			Host:         "127.0.0.1:7373",
			ContentType:  "application/json",
			Body:         "{}",
			ExpectStatus: http.StatusOK,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest(tc.Method, tc.Path, strings.NewReader(tc.Body))
			req.Host = tc.Host

			if tc.ContentType != "" {
				req.Header.Set("Content-Type", tc.ContentType)
			}

			resp := httptest.NewRecorder()
			logger := slog.New(slog.NewTextHandler(t.Output(), nil))

			middleware.Wrap(handler, logger, nil, nil).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
}

// TestWrap_LetsAHandlerFlush covers the capability a streaming response needs from the
// middleware chain, which the chain used to hide.
//
// The logging middleware wraps the writer to record a status. Embedding the interface
// promotes three methods and no more, so a handler asking to flush was told the writer
// could not, and a followed log read then arrived in whatever chunks the connection's
// buffer produced rather than as it was written.
func TestWrap_LetsAHandlerFlush(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var flushed error

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("a line"))
		flushed = http.NewResponseController(w).Flush()
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workloads/example/logs", nil)
	req.Host = "127.0.0.1:7373"

	middleware.Wrap(handler, logger, nil, nil).ServeHTTP(httptest.NewRecorder(), req)

	require.NoError(t, flushed)
}

func TestWrap_RecoveryIsLogged(t *testing.T) {
	t.Parallel()

	// Recovery sits inside Logging so that a panic still leaves a record of the request
	// that caused it. Turning the chain inside out would answer 500 just the same, so
	// the log line is the only thing that says the two are still the right way round.
	var recorded strings.Builder

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
	req.Host = "127.0.0.1:7373"
	resp := httptest.NewRecorder()

	logger := slog.New(slog.NewTextHandler(&recorded, &slog.HandlerOptions{Level: slog.LevelDebug}))

	middleware.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something a handler did not expect")
	}), logger, nil, nil).ServeHTTP(resp, req)

	require.Equal(t, http.StatusInternalServerError, resp.Code)

	logged := recorded.String()
	assert.Contains(t, logged, "handler panicked")
	assert.Contains(t, logged, "something a handler did not expect")
	assert.Contains(t, logged, "http request")
	assert.Contains(t, logged, "status=500")
}
