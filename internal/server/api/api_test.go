package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/api"
)

func TestLimit(t *testing.T) {
	t.Parallel()

	// The handler reads the body the way a real one does, so the limit is exercised
	// where it actually bites rather than asserted on in isolation.
	read := func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		w.WriteHeader(http.StatusOK)
	}

	tt := []struct {
		Name         string
		Body         string
		ExpectStatus int
	}{
		{
			Name:         "accepts a body within the limit",
			Body:         strings.Repeat("a", 1024),
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "refuses a body over the limit",
			// A specification is a hand-written document, so anything past a
			// megabyte is either a mistake or an attempt to see how much the server
			// will hold.
			Body:         strings.Repeat("a", (1<<20)+1),
			ExpectStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/workloads/example", strings.NewReader(tc.Body))
			resp := httptest.NewRecorder()

			api.Limit(http.HandlerFunc(read)).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
}

func TestLimit_NoBody(t *testing.T) {
	t.Parallel()

	// A GET carries no body, and wrapping a nil one would panic rather than limit
	// anything.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
	resp := httptest.NewRecorder()

	var called bool
	api.Limit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})).ServeHTTP(resp, req)

	require.True(t, called)
}

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
			Host:         "orca.evil.example.com:7373",
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

			api.Wrap(handler, logger, nil).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
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

	api.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something a handler did not expect")
	}), logger, nil).ServeHTTP(resp, req)

	require.Equal(t, http.StatusInternalServerError, resp.Code)

	logged := recorded.String()
	assert.Contains(t, logged, "handler panicked")
	assert.Contains(t, logged, "something a handler did not expect")
	assert.Contains(t, logged, "http request")
	assert.Contains(t, logged, "status=500")
}

func TestGuard(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Path         string
		Host         string
		Origin       string
		Permitted    []string
		ExpectStatus int
	}{
		{
			Name:         "accepts the loopback address the server listens on",
			Host:         "127.0.0.1:7373",
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "accepts localhost",
			Host:         "localhost:7373",
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "accepts an address literal on any interface",
			// Reaching orca by an address means the caller knew where it was, and an
			// address is not something an attacker can point at a victim's loopback.
			Host:         "10.0.0.5:7373",
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "accepts an IPv6 literal",
			Host:         "[::1]:7373",
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "refuses a name the operator did not permit",
			// The DNS rebinding case: the attacker owns the name, points it at
			// 127.0.0.1, and the browser sends this on behalf of a page the operator
			// merely visited.
			Host:         "orca.evil.example.com:7373",
			ExpectStatus: http.StatusMisdirectedRequest,
		},
		{
			Name:         "accepts a name the operator permitted",
			Host:         "orca.internal:7373",
			Permitted:    []string{"orca.internal"},
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "compares a permitted name without regard to case",
			Host:         "ORCA.Internal:7373",
			Permitted:    []string{"orca.internal"},
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "refuses an empty host",
			Host:         "",
			ExpectStatus: http.StatusMisdirectedRequest,
		},
		{
			Name: "refuses a scrape addressed by an unpermitted hostname",
			// The guard covers /metrics like everything else: a scraper that
			// targets orca by hostname needs the name in the configuration,
			// while one targeting an address always passes. Pinned here because
			// a scrape failing with a 421 is otherwise confusing to debug.
			Path:         "/metrics",
			Host:         "orca.internal:7373",
			ExpectStatus: http.StatusMisdirectedRequest,
		},
		{
			Name: "refuses a cross-origin request",
			// No page legitimately speaks to this API, so an Origin naming somewhere
			// else is a page acting on its own behalf.
			Host:         "127.0.0.1:7373",
			Origin:       "http://evil.example.com",
			ExpectStatus: http.StatusForbidden,
		},
		{
			Name:         "accepts an origin naming the server itself",
			Host:         "127.0.0.1:7373",
			Origin:       "http://127.0.0.1:7373",
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "refuses an opaque origin",
			// What a sandboxed page sends. It names nowhere this API is served from.
			Host:         "127.0.0.1:7373",
			Origin:       "null",
			ExpectStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			path := tc.Path
			if path == "" {
				path = "/api/v1/workloads"
			}

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Host = tc.Host

			if tc.Origin != "" {
				req.Header.Set("Origin", tc.Origin)
			}

			resp := httptest.NewRecorder()
			logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

			api.Guard(logger, tc.Permitted)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
}

func TestRequireJSON(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Method       string
		ContentType  string
		ExpectStatus int
	}{
		{
			Name:         "accepts a json body",
			Method:       http.MethodPut,
			ContentType:  "application/json",
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "accepts a json body declaring a charset",
			Method:       http.MethodPut,
			ContentType:  "application/json; charset=utf-8",
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "refuses a plain text body",
			// One of the content types a browser sends across origins without asking
			// permission first, which is what makes requiring json worth doing.
			Method:       http.MethodPost,
			ContentType:  "text/plain",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "refuses a form encoded body",
			Method:       http.MethodPost,
			ContentType:  "application/x-www-form-urlencoded",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "refuses a body declaring nothing",
			Method:       http.MethodPut,
			ContentType:  "",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "lets a request with no body through",
			Method:       http.MethodGet,
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "lets a delete through",
			Method:       http.MethodDelete,
			ExpectStatus: http.StatusOK,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest(tc.Method, "/api/v1/workloads/example", strings.NewReader("{}"))
			if tc.ContentType != "" {
				req.Header.Set("Content-Type", tc.ContentType)
			}

			resp := httptest.NewRecorder()

			api.RequireJSON(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
}
