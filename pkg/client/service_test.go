package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// serviceManifest returns a manifest for the example service, selecting
// workloads labelled app=web on 8080/tcp.
func serviceManifest() manifest.Service {
	return manifest.Service{
		Version: "v1",
		Name:    "example",
		Target: manifest.ServiceTarget{
			Labels:   map[string]string{"app": "web"},
			Port:     8080,
			Protocol: manifest.ProtocolTCP,
		},
	}
}

// apiService returns a service as the API reports it, with one backend.
func apiService(name string) api.Service {
	protocol := api.ServiceTargetProtocol("tcp")
	backends := []api.ServiceBackend{
		{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
	}

	return api.Service{
		Name: name,
		Target: api.ServiceTarget{
			Labels:   api.Labels{"app": "web"},
			Port:     8080,
			Protocol: &protocol,
		},
		Backends:  &backends,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

func TestClient_ApplyService(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Handler   http.HandlerFunc
		ExpectErr error
		Assert    func(*testing.T, client.Service)
	}{
		{
			Name: "applies a service that did not exist",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/services/example", r.URL.Path)

				writeJSON(t, w, http.StatusCreated,
					api.ApplyServiceResult{Service: apiService("example")})
			},
			Assert: func(t *testing.T, s client.Service) {
				assert.Equal(t, "example", s.Name)
				assert.Equal(t, manifest.ProtocolTCP, s.Target.Protocol)
				assert.Equal(t, []client.ServiceBackend{
					{Workload: "web", Instance: 0, Address: "203.0.113.10:20000"},
				}, s.Backends)
			},
		},
		{
			Name: "applies a service that already existed",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK,
					api.ApplyServiceResult{Service: apiService("example")})
			},
			Assert: func(t *testing.T, s client.Service) {
				assert.Equal(t, "example", s.Name)
			},
		},
		{
			Name: "reports a service the server will not accept",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid service"})
			},
		},
		{
			Name: "reports an unexpected failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError,
					api.ErrorResponse{Error: "failed to apply service"})
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := newTestClient(t, tc.Handler)

			applied, err := c.ApplyService(t.Context(), serviceManifest())
			switch {
			case tc.ExpectErr != nil:
				assert.ErrorIs(t, err, tc.ExpectErr)
				return
			case tc.Assert == nil:
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			tc.Assert(t, applied)
		})
	}
}

func TestClient_GetService(t *testing.T) {
	t.Parallel()

	t.Run("returns the service", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/services/example", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.GetServiceResult{Service: apiService("example")})
		})

		got, err := c.GetService(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Equal(t, map[string]string{"app": "web"}, got.Target.Labels)
		assert.Len(t, got.Backends, 1)
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound,
				api.ErrorResponse{Error: `service "nope" does not exist`})
		})

		_, err := c.GetService(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrServiceNotFound)
	})

	t.Run("refuses a name that is not a path segment", func(t *testing.T) {
		t.Parallel()

		// Refused before any request is sent: the handler would fail the test.
		c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Error("a request was sent for an unusable name")
		})

		for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
			_, err := c.GetService(t.Context(), name)
			assert.ErrorIs(t, err, client.ErrInvalidServiceName, "accepted the name %q", name)
		}
	})
}

func TestClient_ListServices(t *testing.T) {
	t.Parallel()

	t.Run("returns the matching services", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/services", r.URL.Path)
			assert.Equal(t, []string{"$.labels.app=web"}, r.URL.Query()["query"])

			writeJSON(t, w, http.StatusOK,
				api.ListServicesResult{Services: []api.Service{apiService("example")}})
		})

		services, err := c.ListServices(t.Context(), "$.labels.app=web")
		require.NoError(t, err)

		require.Len(t, services, 1)
		assert.Equal(t, "example", services[0].Name)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid query"})
		})

		_, err := c.ListServices(t.Context(), "nope")
		assert.Error(t, err)
		assert.True(t, client.IsBadRequest(err))
	})
}

func TestClient_StreamServices(t *testing.T) {
	t.Parallel()

	t.Run("reports each set the server writes", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/services", r.URL.Path)
			assert.Equal(t, "true", r.URL.Query().Get("follow"))
			assert.Equal(t, []string{"$.labels.app=web"}, r.URL.Query()["query"])

			w.Header().Set("Content-Type", "application/x-ndjson")

			encoder := json.NewEncoder(w)
			require.NoError(t, encoder.Encode(api.ListServicesResult{Services: []api.Service{apiService("example")}}))
			require.NoError(t, encoder.Encode(api.ListServicesResult{Services: []api.Service{}}))
		})

		var sets [][]client.Service
		err := c.StreamServices(t.Context(), func(services []client.Service) error {
			sets = append(sets, services)
			return nil
		}, "$.labels.app=web")
		require.NoError(t, err)

		// The server ending the stream is not a failure: the caller reconnects if
		// it wants more.
		require.Len(t, sets, 2)
		require.Len(t, sets[0], 1)
		assert.Equal(t, "example", sets[0][0].Name)
		assert.Empty(t, sets[1])
	})

	t.Run("ends with the caller's error", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			require.NoError(t, json.NewEncoder(w).Encode(api.ListServicesResult{Services: []api.Service{}}))
		})

		err := c.StreamServices(t.Context(), func([]client.Service) error {
			return errors.New("seen enough")
		})
		assert.EqualError(t, err, "seen enough")
	})

	t.Run("treats cancellation as the end", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			require.NoError(t, json.NewEncoder(w).Encode(api.ListServicesResult{Services: []api.Service{}}))
			http.NewResponseController(w).Flush()

			// Held open until the caller hangs up, as a quiet fleet would hold it.
			<-r.Context().Done()
		})

		err := c.StreamServices(ctx, func([]client.Service) error {
			cancel()
			return nil
		})
		assert.NoError(t, err)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid query"})
		})

		err := c.StreamServices(t.Context(), func([]client.Service) error {
			t.Fatal("nothing should be reported for a query the server refused")
			return nil
		}, "nope")
		assert.Error(t, err)
		assert.True(t, client.IsBadRequest(err))
	})
}

func TestClient_DeleteService(t *testing.T) {
	t.Parallel()

	t.Run("removes the service", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, "/api/v1/services/example", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.DeleteServiceResult{})
		})

		assert.NoError(t, c.DeleteService(t.Context(), "example"))
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound,
				api.ErrorResponse{Error: `service "nope" does not exist`})
		})

		assert.ErrorIs(t, c.DeleteService(t.Context(), "nope"), client.ErrServiceNotFound)
	})
}
