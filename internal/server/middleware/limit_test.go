package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/middleware"
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

			middleware.Limit(http.HandlerFunc(read)).ServeHTTP(resp, req)

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
	middleware.Limit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})).ServeHTTP(resp, req)

	require.True(t, called)
}
