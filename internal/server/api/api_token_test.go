package api_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
)

func doToken(t *testing.T, tokens api.TokenService, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	mux := http.NewServeMux()
	api.New(api.Config{
		Tokens: api.NewTokenAPI(api.TokenAPIConfig{Logger: logger, Tokens: tokens}),
	}).Register(mux)

	resp := httptest.NewRecorder()
	// Served through the disabled-mode authenticate middleware, so the
	// authorize layer passes the request; its own refusals are tested in
	// api_test.go.
	middleware.Authenticate(nil)(mux).ServeHTTP(resp, req)

	return resp
}

func TestTokenAPI_CreateToken(t *testing.T) {
	t.Parallel()

	t.Run("mints a token and reports the credential once", func(t *testing.T) {
		tokens := NewMockTokenService(t)
		tokens.EXPECT().Create(mock.Anything, "prometheus").Return(service.Token{
			ID:        "id",
			Type:      "client",
			Source:    "static",
			Principal: "prometheus",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}, "takt_c_secret", nil).Once()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"principal": "prometheus"}`))
		req.Header.Set("Content-Type", "application/json")

		resp := doToken(t, tokens, req)
		require.Equal(t, http.StatusCreated, resp.Code)
		assert.JSONEq(t, `{
			"credential": "takt_c_secret",
			"token": {
				"id": "id",
				"type": "client",
				"source": "static",
				"principal": "prometheus",
				"createdAt": "2026-01-01T00:00:00Z"
			}
		}`, resp.Body.String())
	})

	t.Run("refuses a principal the service will not accept", func(t *testing.T) {
		tokens := NewMockTokenService(t)
		tokens.EXPECT().Create(mock.Anything, "group:infra").
			Return(service.Token{}, "", service.ErrInvalidPrincipal).Once()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"principal": "group:infra"}`))
		req.Header.Set("Content-Type", "application/json")

		resp := doToken(t, tokens, req)
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})
}

func TestTokenAPI_ListTokens(t *testing.T) {
	t.Parallel()

	tokens := NewMockTokenService(t)
	tokens.EXPECT().List(mock.Anything).Return([]service.Token{
		{
			ID:        "id",
			Type:      "recovery",
			Source:    "init",
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}, nil).Once()

	resp := doToken(t, tokens, httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil))
	require.Equal(t, http.StatusOK, resp.Code)
	assert.JSONEq(t, `{
		"tokens": [{
			"id": "id",
			"type": "recovery",
			"source": "init",
			"principal": "",
			"createdAt": "2026-01-01T00:00:00Z"
		}]
	}`, resp.Body.String())
}

func TestTokenAPI_DeleteToken(t *testing.T) {
	t.Parallel()

	t.Run("revokes a token", func(t *testing.T) {
		tokens := NewMockTokenService(t)
		tokens.EXPECT().Delete(mock.Anything, "id").Return(nil).Once()

		resp := doToken(t, tokens, httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/id", nil))
		require.Equal(t, http.StatusOK, resp.Code)
		assert.JSONEq(t, `{}`, resp.Body.String())
	})

	t.Run("reports a token that does not exist", func(t *testing.T) {
		tokens := NewMockTokenService(t)
		tokens.EXPECT().Delete(mock.Anything, "id").Return(service.ErrTokenNotFound).Once()

		resp := doToken(t, tokens, httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/id", nil))
		require.Equal(t, http.StatusNotFound, resp.Code)
	})
}
