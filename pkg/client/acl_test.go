package client_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// wirePolicy is the document the acl tests move around, in its wire shape.
func wirePolicy() api.PolicySpec {
	grants := []api.PolicyGrant{
		{Principals: []string{"prometheus"}, Role: api.RoleViewer},
	}

	return api.PolicySpec{Version: "v1", Grants: &grants}
}

// canonicalPolicy is wirePolicy as the client reports it.
func canonicalPolicy() manifest.Policy {
	return manifest.Policy{
		Version: "v1",
		Grants: []manifest.PolicyGrant{
			{Principals: []string{"prometheus"}, Role: manifest.RoleViewer},
		},
	}
}

func TestClient_InitACL(t *testing.T) {
	t.Parallel()

	t.Run("mints the recovery token", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/acl/init", r.URL.Path)

			writeJSON(t, w, http.StatusCreated, api.InitACLResult{Credential: "takt_r_secret"})
		})

		credential, err := c.InitACL(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "takt_r_secret", credential)
	})

	t.Run("reports init has already run", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: "acl is already initialized"})
		})

		_, err := c.InitACL(t.Context())
		assert.ErrorIs(t, err, client.ErrACLInitialized)
	})
}

func TestClient_GetPolicy(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/acl", r.URL.Path)

		w.Header().Set("ETag", `"tag-1"`)
		writeJSON(t, w, http.StatusOK, api.GetACLPolicyResult{Policy: wirePolicy()})
	})

	policy, etag, err := c.GetPolicy(t.Context())
	require.NoError(t, err)
	// The tag is reported exactly as the header carried it, so an apply can
	// present it back without re-quoting.
	assert.Equal(t, `"tag-1"`, etag)
	assert.Equal(t, canonicalPolicy(), policy)
}

func TestClient_ApplyPolicy(t *testing.T) {
	t.Parallel()

	t.Run("replaces the policy against the given tag", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPut, r.Method)
			assert.Equal(t, "/api/v1/acl", r.URL.Path)
			assert.Equal(t, `"tag-1"`, r.Header.Get("If-Match"))

			var body api.PolicySpec
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "v1", body.Version)

			w.Header().Set("ETag", `"tag-2"`)
			writeJSON(t, w, http.StatusOK, api.ApplyACLPolicyResult{Policy: body})
		})

		applied, etag, err := c.ApplyPolicy(t.Context(), canonicalPolicy(), `"tag-1"`)
		require.NoError(t, err)
		assert.Equal(t, `"tag-2"`, etag)
		assert.Equal(t, canonicalPolicy(), applied)
	})

	t.Run("reports a stale tag", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusPreconditionFailed, api.ErrorResponse{Error: "the policy changed since it was read"})
		})

		_, _, err := c.ApplyPolicy(t.Context(), canonicalPolicy(), `"stale"`)
		assert.ErrorIs(t, err, client.ErrPolicyChanged)
	})

	t.Run("refuses an invalid document before sending it", func(t *testing.T) {
		c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Fatal("no request should be sent")
		})

		invalid := manifest.Policy{Version: "v2"}

		_, _, err := c.ApplyPolicy(t.Context(), invalid, `"tag-1"`)
		assert.Error(t, err)
	})
}

func TestClient_ReplacePolicy(t *testing.T) {
	t.Parallel()

	// The get and the conditional apply run as one step, with the tag the
	// get reported travelling into the apply's If-Match.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("ETag", `"tag-1"`)
			writeJSON(t, w, http.StatusOK, api.GetACLPolicyResult{Policy: api.PolicySpec{Version: "v1"}})
		case http.MethodPut:
			assert.Equal(t, `"tag-1"`, r.Header.Get("If-Match"))

			w.Header().Set("ETag", `"tag-2"`)
			writeJSON(t, w, http.StatusOK, api.ApplyACLPolicyResult{Policy: wirePolicy()})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	applied, etag, err := c.ReplacePolicy(t.Context(), canonicalPolicy())
	require.NoError(t, err)
	assert.Equal(t, `"tag-2"`, etag)
	assert.Equal(t, canonicalPolicy(), applied)
}
