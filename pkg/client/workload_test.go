package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestClient_Apply(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name              string
		Handler           http.HandlerFunc
		Assert            func(*testing.T, client.Workload, bool)
		ExpectBadRequest  bool
		ExpectUnsupported bool
	}{
		{
			Name: "creates a workload",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/workloads/example", r.URL.Path)
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

				var got api.WorkloadSpec
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				assert.Equal(t, "example", got.Name)
				require.NotNil(t, got.Container)
				assert.Equal(t, "example/example:latest", got.Container.Image)

				// An empty schedule is omitted rather than sent as "", so the
				// server hashes the same specification the client held.
				assert.Nil(t, got.Schedule)

				writeJSON(t, w, http.StatusCreated, workload("example", api.WorkloadStatePending))
			},
			Assert: func(t *testing.T, got client.Workload, created bool) {
				assert.True(t, created)
				assert.Equal(t, "example", got.Name)
				assert.Equal(t, string(api.WorkloadStatePending), got.State)
			},
		},
		{
			Name: "updates a workload",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, workload("example", api.WorkloadStateRunning))
			},
			Assert: func(t *testing.T, got client.Workload, created bool) {
				assert.False(t, created)
				assert.Equal(t, string(api.WorkloadStateRunning), got.State)
			},
		},
		{
			Name: "reports a rejected specification",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "no runtime specified"})
			},
			ExpectBadRequest: true,
		},
		{
			Name: "reports an unsupported runtime",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusUnprocessableEntity, api.ErrorResponse{Error: "unsupported runtime"})
			},
			ExpectUnsupported: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := newTestClient(t, tc.Handler)

			got, created, err := c.Apply(t.Context(), manifest.Spec{
				Version:   "v1",
				Name:      "example",
				Container: &manifest.Container{Image: "example/example:latest"},
			})

			switch {
			case tc.ExpectBadRequest:
				assert.True(t, client.IsBadRequest(err))
				return
			case tc.ExpectUnsupported:
				assert.True(t, client.IsUnprocessable(err))
				return
			}

			require.NoError(t, err)
			tc.Assert(t, got, created)
		})
	}
}

func TestClient_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/workloads/example", r.URL.Path)

			writeJSON(t, w, http.StatusOK, workload("example", api.WorkloadStateRunning))
		})

		got, err := c.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Equal(t, "v1", got.Spec.Version)
		require.NotNil(t, got.Spec.Container)
		assert.Equal(t, "example/example:latest", got.Spec.Container.Image)
		require.Len(t, got.Instances, 1)
		assert.Equal(t, "container-one", got.Instances[0].ID)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func TestClient_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/workloads", r.URL.Path)

			writeJSON(t, w, http.StatusOK, []api.Workload{
				workload("alpha", api.WorkloadStateRunning),
				workload("bravo", api.WorkloadStatePending),
			})
		})

		got, err := c.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
	})

	t.Run("returns nothing when there are none", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, []api.Workload{})
		})

		got, err := c.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestClient_Delete(t *testing.T) {
	t.Parallel()

	t.Run("deletes the workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, "/api/v1/workloads/example", r.URL.Path)

			w.WriteHeader(http.StatusNoContent)
		})

		require.NoError(t, c.Delete(t.Context(), "example"))
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		assert.ErrorIs(t, c.Delete(t.Context(), "nope"), client.ErrWorkloadNotFound)
	})
}

func TestClient_Logs(t *testing.T) {
	t.Parallel()

	t.Run("returns the logs", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/workloads/example/logs", r.URL.Path)
			assert.Equal(t, "20", r.URL.Query().Get("tail"))

			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello world\n"))
		})

		logs, err := c.Logs(t.Context(), "example", 20)
		require.NoError(t, err)
		assert.Equal(t, "hello world\n", logs)
	})

	t.Run("leaves the limit to the server when tail is zero", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.URL.Query().Get("tail"))

			_, _ = w.Write([]byte("hello world\n"))
		})

		_, err := c.Logs(t.Context(), "example", 0)
		require.NoError(t, err)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `workload "nope" does not exist`})
		})

		_, err := c.Logs(t.Context(), "nope", 0)
		assert.ErrorIs(t, err, client.ErrWorkloadNotFound)
	})
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	c, err := client.New(server.URL)
	require.NoError(t, err)

	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

func workload(name string, state api.WorkloadState) api.Workload {
	now := time.Now().UTC().Truncate(time.Second)

	return api.Workload{
		Name:    name,
		Version: 1,
		Runtime: api.Container,
		State:   state,
		Spec: api.WorkloadSpec{
			Version:   "v1",
			Name:      name,
			Container: &api.ContainerSpec{Image: "example/example:latest"},
		},
		Instances: &[]api.Instance{
			{ID: "container-one", State: api.InstanceStateRunning, SpecHash: "hash-one"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}
