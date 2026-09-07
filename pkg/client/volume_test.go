package client_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestClient_ApplyVolume(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Handler   http.HandlerFunc
		ExpectErr error
		Assert    func(*testing.T, client.Volume)
	}{
		{
			Name: "applies a volume the server created",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/volumes/example-data", r.URL.Path)

				writeJSON(t, w, http.StatusCreated,
					api.ApplyVolumeResult{Volume: apiVolume("example-data")})
			},
			Assert: func(t *testing.T, volume client.Volume) {
				assert.Equal(t, "example-data", volume.Name)
				assert.Equal(t, "/var/lib/takt/volumes/cvhs0dq0kqj4c9r8m1a0", volume.Path)
			},
		},
		{
			Name: "applies a volume the server updated",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK,
					api.ApplyVolumeResult{Volume: apiVolume("example-data")})
			},
			Assert: func(t *testing.T, volume client.Volume) {
				assert.Equal(t, "example-data", volume.Name)
			},
		},
		{
			Name: "reports a name the server will not accept",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid volume"})
			},
		},
		{
			Name: "reports an unexpected failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError,
					api.ErrorResponse{Error: "failed to apply volume"})
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := newTestClient(t, tc.Handler)

			volume, err := c.ApplyVolume(t.Context(), manifest.Volume{Version: "v1", Name: "example-data"})
			switch {
			case tc.ExpectErr != nil:
				assert.ErrorIs(t, err, tc.ExpectErr)

				return
			case tc.Assert == nil:
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			tc.Assert(t, volume)
		})
	}
}

func TestClient_ApplyVolume_CarriesOwnerAndMode(t *testing.T) {
	t.Parallel()

	// The handler decodes what was sent and echoes it back, so this proves the
	// fields cross the wire in both directions.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var spec api.VolumeSpec
		require.NoError(t, json.NewDecoder(r.Body).Decode(&spec))

		require.NotNil(t, spec.Owner)
		assert.Equal(t, "470:470", *spec.Owner)
		require.NotNil(t, spec.Mode)
		assert.Equal(t, "0755", *spec.Mode)

		volume := apiVolume("example-data")
		volume.Owner, volume.Mode = spec.Owner, spec.Mode

		writeJSON(t, w, http.StatusCreated, api.ApplyVolumeResult{Volume: volume})
	})

	volume, err := c.ApplyVolume(t.Context(), manifest.Volume{
		Version: "v1",
		Name:    "example-data",
		Owner:   "470:470",
		Mode:    "0755",
	})
	require.NoError(t, err)

	assert.Equal(t, "470:470", volume.Owner)
	assert.Equal(t, "0755", volume.Mode)
}

func TestClient_GetVolume(t *testing.T) {
	t.Parallel()

	t.Run("returns the volume and what mounts it", func(t *testing.T) {
		volume := apiVolume("example-data")
		volume.UsedBy = &[]string{"alpha", "bravo"}

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/volumes/example-data", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.GetVolumeResult{Volume: volume})
		})

		got, err := c.GetVolume(t.Context(), "example-data")
		require.NoError(t, err)

		assert.Equal(t, "example-data", got.Name)
		assert.Equal(t, []string{"alpha", "bravo"}, got.UsedBy)
		assert.Equal(t, "/var/lib/takt/volumes/cvhs0dq0kqj4c9r8m1a0", got.Path)
	})

	t.Run("reports a volume nothing mounts", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, api.GetVolumeResult{Volume: apiVolume("example-data")})
		})

		got, err := c.GetVolume(t.Context(), "example-data")
		require.NoError(t, err)
		assert.Empty(t, got.UsedBy)
	})

	t.Run("reports a volume that does not exist", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound,
				api.ErrorResponse{Error: `volume "nope" does not exist`})
		})

		_, err := c.GetVolume(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrVolumeNotFound)
	})
}

func TestClient_ListVolumes(t *testing.T) {
	t.Parallel()

	t.Run("returns every volume", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/volumes", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.ListVolumesResult{
				Volumes: []api.Volume{apiVolume("alpha"), apiVolume("bravo")},
			})
		})

		got, err := c.ListVolumes(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
	})

	t.Run("returns nothing when there are none", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, api.ListVolumesResult{Volumes: []api.Volume{}})
		})

		got, err := c.ListVolumes(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("sends queries as repeated parameters", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, []string{"$.labels.app=web", "$.labels.env=prod"}, r.URL.Query()["query"])

			writeJSON(t, w, http.StatusOK, api.ListVolumesResult{Volumes: []api.Volume{}})
		})

		_, err := c.ListVolumes(t.Context(), "$.labels.app=web", "$.labels.env=prod")
		require.NoError(t, err)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid query"})
		})

		_, err := c.ListVolumes(t.Context(), "nonsense")
		assert.True(t, client.IsBadRequest(err))
	})

	t.Run("reports an unexpected failure", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusInternalServerError,
				api.ErrorResponse{Error: "failed to list volumes"})
		})

		_, err := c.ListVolumes(t.Context())
		assert.Error(t, err)
	})
}

func TestClient_DeleteVolume(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Options   []client.DeleteVolumeOption
		Handler   http.HandlerFunc
		ExpectErr error
		Contains  []string
	}{
		{
			Name: "removes the volume",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodDelete, r.Method)
				assert.Equal(t, "/api/v1/volumes/example-data", r.URL.Path)

				// Absent rather than false, so the request says only what it asks for.
				assert.Empty(t, r.URL.Query().Get("force"))

				writeJSON(t, w, http.StatusOK, map[string]any{})
			},
		},
		{
			Name:    "forces the deletion when asked",
			Options: []client.DeleteVolumeOption{client.WithForce()},
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "true", r.URL.Query().Get("force"))

				writeJSON(t, w, http.StatusOK, map[string]any{})
			},
		},
		{
			Name: "reports a volume a workload mounts",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusConflict,
					api.ErrorResponse{Error: "volume is in use: mounted by alpha, bravo"})
			},
			ExpectErr: client.ErrVolumeInUse,
			// The names come through, because the caller's next question is which
			// workloads are holding it.
			Contains: []string{"alpha", "bravo"},
		},
		{
			Name: "reports a volume that does not exist",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusNotFound,
					api.ErrorResponse{Error: `volume "nope" does not exist`})
			},
			ExpectErr: client.ErrVolumeNotFound,
		},
		{
			Name: "reports an unexpected failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError,
					api.ErrorResponse{Error: "failed to delete volume"})
			},
			Contains: []string{"failed to delete volume"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := newTestClient(t, tc.Handler)

			err := c.DeleteVolume(t.Context(), "example-data", tc.Options...)
			switch {
			case tc.ExpectErr != nil:
				require.ErrorIs(t, err, tc.ExpectErr)
			case len(tc.Contains) > 0:
				require.Error(t, err)
			default:
				assert.NoError(t, err)
			}

			for _, want := range tc.Contains {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// TestClient_VolumeNameWithASlashIsRefused covers a name that is not a single path
// segment.
//
// Such a name cannot arrive from a manifest or the CLI, both of which validate it, but
// this is a public package and a caller can pass anything. It matters because the name
// is interpolated into the request path: Go's client resolves "." and ".." before
// sending, so a name containing them would otherwise reach whichever endpoint the
// resolved path names — including a workload's.
func TestClient_VolumeNameWithASlashIsRefused(t *testing.T) {
	t.Parallel()

	var requested string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.EscapedPath()

		writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: "volume not found"})
	})

	for _, name := range []string{"../workloads/example", "a/b", "..", "."} {
		requested = ""

		_, err := c.GetVolume(t.Context(), name)
		require.Error(t, err, "accepted the name %q", name)
		assert.ErrorIs(t, err, client.ErrInvalidVolumeName)

		assert.Empty(t, requested, "the name %q was sent to the server", name)
	}
}

func apiVolume(name string) api.Volume {
	path := "/var/lib/takt/volumes/cvhs0dq0kqj4c9r8m1a0"

	return api.Volume{
		Name:      name,
		Path:      &path,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
}
