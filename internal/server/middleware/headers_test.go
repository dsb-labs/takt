package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/internal/server/middleware"
)

func TestHeaders(t *testing.T) {
	t.Parallel()

	serve := func(tls bool) http.Header {
		handler := middleware.Headers(tls)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/", nil))

		return resp.Header()
	}

	t.Run("refuses framing and sniffing on every response", func(t *testing.T) {
		header := serve(false)

		assert.Equal(t, "DENY", header.Get("X-Frame-Options"))
		assert.Equal(t, "nosniff", header.Get("X-Content-Type-Options"))
		assert.Contains(t, header.Get("Content-Security-Policy"), "frame-ancestors 'none'")
		assert.Contains(t, header.Get("Content-Security-Policy"), "script-src 'self'")
	})

	t.Run("insists on transport security only over tls", func(t *testing.T) {
		// A browser told to insist on TLS for a name it reached over plain HTTP
		// would refuse the name from then on.
		assert.Empty(t, serve(false).Get("Strict-Transport-Security"))
		assert.NotEmpty(t, serve(true).Get("Strict-Transport-Security"))
	})
}
