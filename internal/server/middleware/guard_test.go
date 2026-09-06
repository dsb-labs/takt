package middleware_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/internal/server/middleware"
)

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
			// Reaching takt by an address means the caller knew where it was, and an
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
			Host:         "takt.evil.example.com:7373",
			ExpectStatus: http.StatusMisdirectedRequest,
		},
		{
			Name:         "accepts a name the operator permitted",
			Host:         "takt.internal:7373",
			Permitted:    []string{"takt.internal"},
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "compares a permitted name without regard to case",
			Host:         "TAKT.Internal:7373",
			Permitted:    []string{"takt.internal"},
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
			// targets takt by hostname needs the name in the configuration,
			// while one targeting an address always passes. Pinned here because
			// a scrape failing with a 421 is otherwise confusing to debug.
			Path:         "/api/v1/metrics",
			Host:         "takt.internal:7373",
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

			middleware.Guard(logger, tc.Permitted)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
}
