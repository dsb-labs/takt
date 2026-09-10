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
)

func TestClient_CreateToken(t *testing.T) {
	t.Parallel()

	created := time.Now().UTC().Truncate(time.Second)

	t.Run("mints a token for a principal", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/tokens", r.URL.Path)

			var body api.CreateTokenRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "prometheus", body.Principal)

			writeJSON(t, w, http.StatusCreated, api.CreateTokenResult{
				Credential: "takt_c_secret",
				Token: api.Token{
					ID:        "id",
					Type:      api.TokenTypeClient,
					Source:    api.TokenSourceStatic,
					Principal: "prometheus",
					CreatedAt: created,
				},
			})
		})

		token, credential, err := c.CreateToken(t.Context(), "prometheus")
		require.NoError(t, err)
		assert.Equal(t, "takt_c_secret", credential)
		assert.Equal(t, client.Token{
			ID:        "id",
			Type:      "client",
			Source:    "static",
			Principal: "prometheus",
			CreatedAt: created,
		}, token)
	})

	t.Run("reports a principal the server refused", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid principal"})
		})

		_, _, err := c.CreateToken(t.Context(), "group:infra")
		assert.True(t, client.IsBadRequest(err))
	})
}

func TestClient_ListTokens(t *testing.T) {
	t.Parallel()

	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/tokens", r.URL.Path)

		writeJSON(t, w, http.StatusOK, api.ListTokensResult{
			Tokens: []api.Token{
				{
					ID:        "id",
					Type:      api.TokenTypeClient,
					Source:    api.TokenSourceSession,
					Principal: "david@dsb.dev",
					ExpiresAt: new(expires),
				},
			},
		})
	})

	tokens, err := c.ListTokens(t.Context())
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	assert.Equal(t, "session", tokens[0].Source)
	assert.Equal(t, expires, tokens[0].ExpiresAt)
	assert.True(t, tokens[0].LastUsedAt.IsZero())
}

func TestClient_DeleteToken(t *testing.T) {
	t.Parallel()

	t.Run("revokes a token", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, "/api/v1/tokens/id", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.DeleteTokenResult{})
		})

		assert.NoError(t, c.DeleteToken(t.Context(), "id"))
	})

	t.Run("reports a token that does not exist", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: "token does not exist"})
		})

		assert.ErrorIs(t, c.DeleteToken(t.Context(), "id"), client.ErrTokenNotFound)
	})

	t.Run("refuses an identifier that is not a path segment", func(t *testing.T) {
		c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Fatal("no request should be sent")
		})

		assert.ErrorIs(t, c.DeleteToken(t.Context(), "../secrets"), client.ErrInvalidTokenID)
	})
}
