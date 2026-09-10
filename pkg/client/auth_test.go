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

func TestClient_WhoAmI(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name      string
		Handler   http.HandlerFunc
		Expected  client.Identity
		ExpectErr bool
	}{
		{
			Name: "reports the caller's identity",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/auth", r.URL.Path)

				writeJSON(t, w, http.StatusOK, api.GetAuthResult{
					Enabled:   true,
					Principal: "david@dsb.dev",
					Role:      "admin",
					Groups:    []string{"infra"},
				})
			},
			Expected: client.Identity{
				Enabled:   true,
				Principal: "david@dsb.dev",
				Role:      "admin",
				Groups:    []string{"infra"},
			},
		},
		{
			Name: "reports a failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed"})
			},
			ExpectErr: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			identity, err := newTestClient(t, tc.Handler).WhoAmI(t.Context())
			if tc.ExpectErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, identity)
		})
	}
}

func TestClient_Login(t *testing.T) {
	t.Parallel()

	expires := time.Now().UTC().Add(12 * time.Hour).Truncate(time.Second)

	tt := []struct {
		Name               string
		Handler            http.HandlerFunc
		Expected           client.Login
		ExpectOIDCMissing  bool
		ExpectUnauthorized bool
	}{
		{
			Name: "exchanges an identity for a token",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/auth", r.URL.Path)

				var body api.LoginRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.NotNil(t, body.IDToken)
				assert.Equal(t, "raw", *body.IDToken)
				assert.Nil(t, body.Token)

				writeJSON(t, w, http.StatusOK, api.LoginResult{
					Credential: "takt_c_secret",
					Principal:  "david@dsb.dev",
					ExpiresAt:  expires,
				})
			},
			Expected: client.Login{
				Credential: "takt_c_secret",
				Principal:  "david@dsb.dev",
				ExpiresAt:  expires,
			},
		},
		{
			Name: "reports oidc is not configured",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "oidc is not configured"})
			},
			ExpectOIDCMissing: true,
		},
		{
			Name: "reports an identity the server refused",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusUnauthorized, api.ErrorResponse{Error: "invalid credential"})
			},
			ExpectUnauthorized: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			login, err := newTestClient(t, tc.Handler).Login(t.Context(), "raw")
			switch {
			case tc.ExpectOIDCMissing:
				assert.ErrorIs(t, err, client.ErrOIDCNotConfigured)

				return
			case tc.ExpectUnauthorized:
				assert.True(t, client.IsUnauthorized(err))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, login)
		})
	}
}

func TestClient_LoginCode(t *testing.T) {
	t.Parallel()

	t.Run("sends the code exchange", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/auth", r.URL.Path)

			var body api.LoginRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotNil(t, body.Code)
			require.NotNil(t, body.Verifier)
			require.NotNil(t, body.RedirectURI)
			assert.Equal(t, "abc", *body.Code)
			assert.Nil(t, body.IDToken)

			writeJSON(t, w, http.StatusOK, api.LoginResult{
				Credential: "takt_c_secret",
				Principal:  "david@dsb.dev",
			})
		})

		login, err := c.LoginCode(t.Context(), "abc", "verifier", "http://127.0.0.1:8250/oidc/callback")
		require.NoError(t, err)
		assert.Equal(t, "takt_c_secret", login.Credential)
	})

	t.Run("reports a refused exchange", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusUnauthorized, api.ErrorResponse{Error: "invalid credential"})
		})

		_, err := c.LoginCode(t.Context(), "abc", "verifier", "http://127.0.0.1:8250/oidc/callback")
		assert.True(t, client.IsUnauthorized(err))
	})
}

func TestClient_Logout(t *testing.T) {
	t.Parallel()

	t.Run("revokes the credential", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Equal(t, "/api/v1/auth", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.LogoutResult{})
		})

		assert.NoError(t, c.Logout(t.Context()))
	})

	t.Run("reports the recovery token's refusal", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusConflict, api.ErrorResponse{Error: "the recovery token is revoked by the reset file"})
		})

		assert.ErrorIs(t, c.Logout(t.Context()), client.ErrRecoveryLogout)
	})
}

func TestClient_GetOIDC(t *testing.T) {
	t.Parallel()

	t.Run("reports the issuer and client", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/auth/oidc", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.GetOIDCResult{Issuer: "https://idp.example.com", ClientID: "takt"})
		})

		oidc, err := c.GetOIDC(t.Context())
		require.NoError(t, err)
		assert.Equal(t, client.OIDC{Issuer: "https://idp.example.com", ClientID: "takt"}, oidc)
	})

	t.Run("reports oidc is not configured", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: "oidc is not configured"})
		})

		_, err := c.GetOIDC(t.Context())
		assert.ErrorIs(t, err, client.ErrOIDCNotConfigured)
	})
}
