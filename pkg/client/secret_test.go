package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/client"
)

func TestClient_SetSecret(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name          string
		Handler       http.HandlerFunc
		ExpectErr     func(error) bool
		ExpectCreated bool
		Assert        func(*testing.T, client.Secret)
	}{
		{
			Name: "creates a secret",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/secrets/db-password", r.URL.Path)

				var body api.SecretSpec
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, "hunter2", body.Value)

				writeJSON(t, w, http.StatusCreated, api.SetSecretResult{Secret: apiSecret("db-password")})
			},
			ExpectCreated: true,
			Assert: func(t *testing.T, secret client.Secret) {
				assert.Equal(t, "db-password", secret.Name)
				assert.Equal(t, "9f2c4a1e8b7d3f6002a5c8e1b4d7f0a3", secret.Revision)
			},
		},
		{
			Name: "updates a secret",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, api.SetSecretResult{Secret: apiSecret("db-password")})
			},
			ExpectCreated: false,
		},
		{
			Name: "reports a name the server will not accept",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid secret"})
			},
			ExpectErr: client.IsBadRequest,
		},
		{
			Name: "reports a failure to store",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError,
					api.ErrorResponse{Error: "failed to set secret"})
			},
			ExpectErr: func(err error) bool { return err != nil },
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			secret, created, err := newTestClient(t, tc.Handler).
				SetSecret(t.Context(), "db-password", []byte("hunter2"), nil)
			if tc.ExpectErr != nil {
				assert.True(t, tc.ExpectErr(err), "unexpected error: %v", err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.ExpectCreated, created)

			if tc.Assert != nil {
				tc.Assert(t, secret)
			}
		})
	}
}

func TestClient_GetSecret(t *testing.T) {
	t.Parallel()

	t.Run("returns the secret", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/secrets/db-password", r.URL.Path)

			stored := apiSecret("db-password")
			stored.UsedBy = new([]string{"example"})

			writeJSON(t, w, http.StatusOK, api.GetSecretResult{Secret: stored})
		})

		secret, err := c.GetSecret(t.Context(), "db-password")
		require.NoError(t, err)
		assert.Equal(t, "db-password", secret.Name)
		assert.Equal(t, []string{"example"}, secret.UsedBy)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `secret "nope" does not exist`})
		})

		_, err := c.GetSecret(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrSecretNotFound)
	})

	t.Run("refuses a name that is not one path segment", func(t *testing.T) {
		// Go's HTTP client resolves dot segments before sending, so a name carrying one
		// would reach whichever endpoint the resolved path names.
		_, err := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Error("the server was reached with an unusable name")
		}).GetSecret(t.Context(), "../workloads")
		assert.ErrorIs(t, err, client.ErrInvalidSecretName)
	})
}

func TestClient_ListSecrets(t *testing.T) {
	t.Parallel()

	t.Run("returns every secret", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/secrets", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.ListSecretsResult{
				Secrets: []api.Secret{apiSecret("api-token"), apiSecret("db-password")},
			})
		})

		secrets, err := c.ListSecrets(t.Context())
		require.NoError(t, err)
		require.Len(t, secrets, 2)
		assert.Equal(t, "api-token", secrets[0].Name)
	})

	t.Run("returns nothing when none are stored", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, api.ListSecretsResult{})
		})

		secrets, err := c.ListSecrets(t.Context())
		require.NoError(t, err)
		assert.Empty(t, secrets)
	})

	t.Run("sends queries as repeated parameters", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, []string{"$.labels.app=web", "$.labels.env=prod"}, r.URL.Query()["query"])

			writeJSON(t, w, http.StatusOK, api.ListSecretsResult{})
		})

		_, err := c.ListSecrets(t.Context(), "$.labels.app=web", "$.labels.env=prod")
		require.NoError(t, err)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid query"})
		})

		_, err := c.ListSecrets(t.Context(), "nonsense")
		assert.True(t, client.IsBadRequest(err))
	})
}

func TestClient_DeleteSecret(t *testing.T) {
	t.Parallel()

	t.Run("removes a secret nothing reads", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Empty(t, r.URL.Query().Get("force"))

			writeJSON(t, w, http.StatusOK, api.DeleteSecretResult{})
		})

		require.NoError(t, c.DeleteSecret(t.Context(), "db-password"))
	})

	t.Run("refuses one a workload reads", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusConflict,
				api.ErrorResponse{Error: "secret is in use: read by example"})
		})

		err := c.DeleteSecret(t.Context(), "db-password")
		require.ErrorIs(t, err, client.ErrSecretInUse)

		// The server's message names the workloads, so the caller can act on it.
		assert.Contains(t, err.Error(), "example")
	})

	t.Run("forces one a workload reads", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "true", r.URL.Query().Get("force"))

			writeJSON(t, w, http.StatusOK, api.DeleteSecretResult{})
		})

		require.NoError(t, c.DeleteSecret(t.Context(), "db-password", client.WithForceDelete()))
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `secret "nope" does not exist`})
		})

		assert.ErrorIs(t, c.DeleteSecret(t.Context(), "nope"), client.ErrSecretNotFound)
	})
}

func TestSecretCarriesNoValue(t *testing.T) {
	t.Parallel()

	// The server sends no value, but a caller could still be handed one by a proxy or
	// a future version. The client's own type has nowhere to put it, which this pins
	// by decoding a body that carries one.
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		_, err := io.WriteString(w, `{"secret":{"name":"db-password","revision":"rev-one",`+
			`"value":"hunter2","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}}`)
		require.NoError(t, err)
	})

	secret, err := c.GetSecret(t.Context(), "db-password")
	require.NoError(t, err)
	assert.Equal(t, "db-password", secret.Name)
	assert.Equal(t, "rev-one", secret.Revision)
}

func apiSecret(name string) api.Secret {
	now := time.Now().UTC().Truncate(time.Second)

	return api.Secret{
		Name:      name,
		Revision:  "9f2c4a1e8b7d3f6002a5c8e1b4d7f0a3",
		CreatedAt: now,
		UpdatedAt: now,
	}
}
