package client_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/client"
)

func TestClient_Health(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Handler      http.HandlerFunc
		ExpectsError bool
	}{
		{
			Name: "healthy",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/system/health", r.URL.Path)

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(api.GetHealthResult{Status: api.Ok})
			},
		},
		{
			Name: "server failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "broken"})
			},
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			server := httptest.NewServer(tc.Handler)
			t.Cleanup(server.Close)

			c, err := client.New(server.URL)
			require.NoError(t, err)

			err = c.Health(t.Context())
			if tc.ExpectsError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
		})
	}
}

func TestClient_Ready(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Handler  http.HandlerFunc
		Expected client.Readiness
	}{
		{
			Name: "ready",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/system/ready", r.URL.Path)

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(api.GetReadinessResult{Ready: true})
			},
			Expected: client.Readiness{Ready: true},
		},
		{
			// A 503 is the server answering "not yet", not the request failing,
			// so it comes back as a value the caller can act on.
			Name: "not ready",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(api.GetReadinessResult{
					Ready:   false,
					Reasons: &[]string{"driver container: daemon gone"},
				})
			},
			Expected: client.Readiness{Ready: false, Reasons: []string{"driver container: daemon gone"}},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			server := httptest.NewServer(tc.Handler)
			t.Cleanup(server.Close)

			c, err := client.New(server.URL)
			require.NoError(t, err)

			readiness, err := c.Ready(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.Expected, readiness)
		})
	}
}

func TestClient_Metrics(t *testing.T) {
	t.Parallel()

	t.Run("copies the scrape to the writer", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/system/metrics", r.URL.Path)

			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte("takt_reconcile_passes_total{outcome=\"ok\"} 3\n"))
		}))
		t.Cleanup(server.Close)

		c, err := client.New(server.URL)
		require.NoError(t, err)

		var out bytes.Buffer
		require.NoError(t, c.Metrics(t.Context(), &out))
		assert.Contains(t, out.String(), "takt_reconcile_passes_total")
	})

	t.Run("reports a failure with the server's message", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "failed to gather metrics"})
		}))
		t.Cleanup(server.Close)

		c, err := client.New(server.URL)
		require.NoError(t, err)

		var out bytes.Buffer
		err = c.Metrics(t.Context(), &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to gather metrics")
		assert.Zero(t, out.Len(), "nothing is written on a failed scrape")
	})
}
