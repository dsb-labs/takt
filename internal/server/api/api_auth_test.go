package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The identityStub type authenticates every credential as one fixed identity,
// which lets a test place a caller behind the middleware the handlers read.
type identityStub struct {
	identity auth.Identity
}

func (s identityStub) Authenticate(context.Context, string) (auth.Identity, error) {
	return s.identity, nil
}

// doAuth serves req through the auth API behind the authenticate middleware.
// A nil authenticator is the disabled mode, exactly as Wrap treats it.
func doAuth(t *testing.T, svc api.AuthService, oidc *api.OIDCRelyingParty, authenticator middleware.Authenticator, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	mux := http.NewServeMux()
	api.New(api.Config{
		Auth: api.NewAuthAPI(api.AuthAPIConfig{Logger: logger, Auth: svc, OIDC: oidc}),
	}).Register(mux)

	resp := httptest.NewRecorder()
	middleware.Authenticate(authenticator)(mux).ServeHTTP(resp, req)

	return resp
}

func TestAuthAPI_GetAuth(t *testing.T) {
	t.Parallel()

	t.Run("reports the caller's identity", func(t *testing.T) {
		caller := identityStub{identity: auth.Identity{
			Principal: "david@dsb.dev",
			Groups:    []string{"infra"},
			Role:      manifest.RoleAdmin,
			TokenID:   "id",
		}}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth", nil)
		req.Header.Set("Authorization", "Bearer anything")

		resp := doAuth(t, NewMockAuthService(t), nil, caller, req)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.JSONEq(t, `{
			"enabled": true,
			"principal": "david@dsb.dev",
			"role": "admin",
			"groups": ["infra"],
			"recovery": false
		}`, resp.Body.String())
	})

	t.Run("reports the disabled mode as anonymous admin", func(t *testing.T) {
		resp := doAuth(t, NewMockAuthService(t), nil, nil,
			httptest.NewRequest(http.MethodGet, "/api/v1/auth", nil))

		require.Equal(t, http.StatusOK, resp.Code)
		assert.JSONEq(t, `{
			"enabled": false,
			"principal": "anonymous",
			"role": "admin",
			"groups": [],
			"recovery": false
		}`, resp.Body.String())
	})
}

func TestAuthAPI_Login(t *testing.T) {
	t.Parallel()

	minted := service.Token{
		ID:        "session-id",
		Type:      "client",
		Source:    "session",
		Principal: "prometheus",
		ExpiresAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	login := func(t *testing.T, svc api.AuthService, body string) *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		return doAuth(t, svc, nil, identityStub{}, req)
	}

	t.Run("exchanges a client token for a session", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().LoginToken(mock.Anything, "takt_c_secret").Return(minted, "takt_c_session", nil).Once()

		resp := login(t, svc, `{"token": "takt_c_secret"}`)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Empty(t, resp.Header().Get("Set-Cookie"))
		assert.JSONEq(t, `{
			"credential": "takt_c_session",
			"principal": "prometheus",
			"expiresAt": "2026-01-01T12:00:00Z"
		}`, resp.Body.String())
	})

	t.Run("sets the session cookie when asked to", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().LoginToken(mock.Anything, "takt_c_secret").Return(minted, "takt_c_session", nil).Once()

		resp := login(t, svc, `{"token": "takt_c_secret", "cookie": true}`)
		require.Equal(t, http.StatusOK, resp.Code)

		cookie := resp.Header().Get("Set-Cookie")
		assert.Contains(t, cookie, auth.SessionCookie+"=takt_c_session")
		assert.Contains(t, cookie, "HttpOnly")
	})

	t.Run("exchanges an oidc identity", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().LoginOIDC(mock.Anything, "raw").Return(minted, "takt_c_session", nil).Once()

		resp := login(t, svc, `{"idToken": "raw"}`)
		require.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("refuses a body naming both exchanges", func(t *testing.T) {
		resp := login(t, NewMockAuthService(t), `{"idToken": "raw", "token": "takt_c_secret"}`)
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("refuses a body naming neither exchange", func(t *testing.T) {
		resp := login(t, NewMockAuthService(t), `{}`)
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("reports oidc is not configured", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().LoginOIDC(mock.Anything, "raw").
			Return(service.Token{}, "", service.ErrOIDCDisabled).Once()

		resp := login(t, svc, `{"idToken": "raw"}`)
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("refuses an invalid credential", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().LoginToken(mock.Anything, "revoked").
			Return(service.Token{}, "", service.ErrInvalidCredential).Once()

		resp := login(t, svc, `{"token": "revoked"}`)
		require.Equal(t, http.StatusUnauthorized, resp.Code)
	})
}

func TestAuthAPI_Logout(t *testing.T) {
	t.Parallel()

	caller := auth.Identity{Principal: "prometheus", Role: manifest.RoleViewer, TokenID: "id"}

	logout := func(t *testing.T, svc api.AuthService, identity auth.Identity) *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth", nil)
		req.Header.Set("Authorization", "Bearer anything")

		return doAuth(t, svc, nil, identityStub{identity: identity}, req)
	}

	t.Run("revokes the caller's credential and clears the cookie", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().Logout(mock.Anything, caller).Return(nil).Once()

		resp := logout(t, svc, caller)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Contains(t, resp.Header().Get("Set-Cookie"), auth.SessionCookie+"=;")
	})

	t.Run("refuses the recovery token", func(t *testing.T) {
		recovery := auth.Identity{Role: manifest.RoleAdmin, TokenID: "id", Recovery: true}

		svc := NewMockAuthService(t)
		svc.EXPECT().Logout(mock.Anything, recovery).Return(service.ErrRecoveryLogout).Once()

		resp := logout(t, svc, recovery)
		require.Equal(t, http.StatusConflict, resp.Code)
	})
}

func TestAuthAPI_GetOIDC(t *testing.T) {
	t.Parallel()

	t.Run("reports the issuer and client", func(t *testing.T) {
		oidc := &api.OIDCRelyingParty{Issuer: "https://issuer.example.com", ClientID: "takt"}

		resp := doAuth(t, NewMockAuthService(t), oidc, identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc", nil))

		require.Equal(t, http.StatusOK, resp.Code)
		assert.JSONEq(t, `{"issuer": "https://issuer.example.com", "clientId": "takt"}`, resp.Body.String())
	})

	t.Run("answers 404 when oidc is not configured", func(t *testing.T) {
		resp := doAuth(t, NewMockAuthService(t), nil, identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc", nil))

		require.Equal(t, http.StatusNotFound, resp.Code)
	})
}

func TestAuthAPI_OIDCFlow(t *testing.T) {
	t.Parallel()

	minted := service.Token{
		Principal: "david@dsb.dev",
		ExpiresAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}

	// The token endpoint the callback exchanges its code against, standing in
	// for the issuer.
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access",
			"token_type":   "bearer",
			"id_token":     "raw-identity",
		})
	}))
	t.Cleanup(issuer.Close)

	relyingParty := func() *api.OIDCRelyingParty {
		return &api.OIDCRelyingParty{
			Issuer:   issuer.URL,
			ClientID: "takt",
			Flow: &oauth2.Config{
				ClientID:    "takt",
				Endpoint:    oauth2.Endpoint{AuthURL: issuer.URL + "/authorize", TokenURL: issuer.URL + "/token"},
				RedirectURL: "https://takt.example.com/api/v1/auth/oidc/callback",
				Scopes:      []string{"openid", "email"},
			},
		}
	}

	t.Run("the login redirect carries the state cookie", func(t *testing.T) {
		resp := doAuth(t, NewMockAuthService(t), relyingParty(), identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil))

		require.Equal(t, http.StatusFound, resp.Code)

		location, err := url.Parse(resp.Header().Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, issuer.URL+"/authorize", location.Scheme+"://"+location.Host+location.Path)
		assert.NotEmpty(t, location.Query().Get("state"))
		assert.NotEmpty(t, location.Query().Get("code_challenge"))
		assert.Contains(t, resp.Header().Get("Set-Cookie"), "takt_oidc=")
	})

	t.Run("the callback completes the flow the login started", func(t *testing.T) {
		svc := NewMockAuthService(t)
		svc.EXPECT().LoginOIDC(mock.Anything, "raw-identity").Return(minted, "takt_c_session", nil).Once()

		started := doAuth(t, NewMockAuthService(t), relyingParty(), identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil))
		require.Equal(t, http.StatusFound, started.Code)

		location, err := url.Parse(started.Header().Get("Location"))
		require.NoError(t, err)

		cookie := started.Result().Cookies()[0]
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/auth/oidc/callback?code=abc&state="+location.Query().Get("state"), nil)
		req.AddCookie(cookie)

		resp := doAuth(t, svc, relyingParty(), identityStub{}, req)
		require.Equal(t, http.StatusFound, resp.Code)
		assert.Equal(t, "/", resp.Header().Get("Location"))
		assert.Contains(t, resp.Header().Get("Set-Cookie"), auth.SessionCookie+"=takt_c_session")
	})

	t.Run("the callback refuses a state it did not issue", func(t *testing.T) {
		started := doAuth(t, NewMockAuthService(t), relyingParty(), identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil))
		require.Equal(t, http.StatusFound, started.Code)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=abc&state=forged", nil)
		req.AddCookie(started.Result().Cookies()[0])

		resp := doAuth(t, NewMockAuthService(t), relyingParty(), identityStub{}, req)
		require.Equal(t, http.StatusUnauthorized, resp.Code)
	})

	t.Run("the callback refuses a request without the cookie", func(t *testing.T) {
		resp := doAuth(t, NewMockAuthService(t), relyingParty(), identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=abc&state=x", nil))

		require.Equal(t, http.StatusUnauthorized, resp.Code)
	})

	t.Run("the browser flow answers 404 without a redirect url", func(t *testing.T) {
		oidc := &api.OIDCRelyingParty{Issuer: issuer.URL, ClientID: "takt"}

		resp := doAuth(t, NewMockAuthService(t), oidc, identityStub{},
			httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil))

		require.Equal(t, http.StatusNotFound, resp.Code)
	})
}
