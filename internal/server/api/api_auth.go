package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The AuthService interface describes the authentication operations the
	// API exposes.
	AuthService interface {
		// LoginOIDC should exchange a verified OIDC identity for a
		// short-lived client token.
		LoginOIDC(ctx context.Context, rawIDToken string) (service.Token, string, error)
		// LoginToken should exchange an existing client token for a
		// short-lived session token bound to the same principal.
		LoginToken(ctx context.Context, credential string) (service.Token, string, error)
		// Logout should revoke the credential behind the given identity.
		Logout(ctx context.Context, identity auth.Identity) error
	}

	// The OIDCRelyingParty type carries what the browser OIDC flow runs
	// against. The discovery endpoint needs only the issuer and client
	// identifier. The redirect flow additionally needs Flow, which is absent
	// when the configuration names no redirect URL.
	OIDCRelyingParty struct {
		// The issuer the authorization code flow runs against.
		Issuer string
		// The client identifier registered with the issuer.
		ClientID string
		// The authorization code flow the login redirect starts and the
		// callback completes. Nil disables the browser flow while the CLI's
		// own loopback flow keeps working.
		Flow *oauth2.Config
	}

	// The AuthAPI type exposes HTTP endpoints for the caller's own
	// authentication.
	AuthAPI struct {
		logger *slog.Logger
		svc    AuthService
		oidc   *OIDCRelyingParty
		secure bool
	}

	// The AuthAPIConfig type contains fields used to construct an AuthAPI.
	AuthAPIConfig struct {
		// The logger used to record failures the response deliberately
		// doesn't describe.
		Logger *slog.Logger
		// The service performing the authentication operations.
		Auth AuthService
		// The OIDC configuration the browser flow and discovery answer
		// from. May be nil, in which case the OIDC routes answer 404 and a
		// pasted token is the only way in.
		OIDC *OIDCRelyingParty
		// Whether cookies are marked Secure, which follows whether the
		// server terminates TLS itself.
		Secure bool
	}
)

// How long the browser has to complete a login redirect before the state
// cookie expires.
const oidcStateTTL = 10 * time.Minute

// NewAuthAPI returns a new instance of the AuthAPI type.
func NewAuthAPI(config AuthAPIConfig) *AuthAPI {
	return &AuthAPI{
		logger: config.Logger.With("component", "api"),
		svc:    config.Auth,
		oidc:   config.OIDC,
		secure: config.Secure,
	}
}

// GetAuth reports who the caller is.
func (a *AuthAPI) GetAuth(ctx context.Context, _ api.GetAuthRequestObject) (api.GetAuthResponseObject, error) {
	identity := middleware.CallerIdentity(ctx)

	if identity.Disabled {
		return api.GetAuth200JSONResponse{
			Enabled:   false,
			Principal: "anonymous",
			Role:      string(manifest.RoleAdmin),
			Groups:    []string{},
		}, nil
	}

	groups := identity.Groups
	if groups == nil {
		groups = []string{}
	}

	return api.GetAuth200JSONResponse{
		Enabled:   true,
		Principal: identity.Principal,
		Role:      string(identity.Role),
		Groups:    groups,
		Recovery:  identity.Recovery,
	}, nil
}

// Login exchanges an identity for a short-lived client token.
func (a *AuthAPI) Login(ctx context.Context, request api.LoginRequestObject) (api.LoginResponseObject, error) {
	if request.Body == nil {
		return api.Login400JSONResponse{
			Error: "request body is required",
		}, nil
	}

	var (
		token      service.Token
		credential string
		err        error
	)

	switch {
	case request.Body.IDToken != nil && request.Body.Token == nil:
		token, credential, err = a.svc.LoginOIDC(ctx, *request.Body.IDToken)
	case request.Body.Token != nil && request.Body.IDToken == nil:
		token, credential, err = a.svc.LoginToken(ctx, *request.Body.Token)
	default:
		return api.Login400JSONResponse{
			Error: "exactly one of idToken and token is required",
		}, nil
	}

	switch {
	case errors.Is(err, service.ErrOIDCDisabled):
		return api.Login400JSONResponse{
			Error: "oidc is not configured",
		}, nil
	case errors.Is(err, service.ErrInvalidCredential):
		return api.Login401JSONResponse{
			Body: api.ErrorResponse{Error: "invalid credential"},
		}, nil
	case err != nil:
		return api.Login500JSONResponse{
			Error: internalError(a.logger, "login", err),
		}, nil
	}

	response := api.Login200JSONResponse{
		Body: api.LoginResult{
			Credential: credential,
			Principal:  token.Principal,
			ExpiresAt:  token.ExpiresAt,
		},
	}

	if request.Body.Cookie != nil && *request.Body.Cookie {
		response.Headers.SetCookie = new(a.sessionCookie(credential, token.ExpiresAt).String())
	}

	return response, nil
}

// Logout revokes the credential that authenticated this request.
func (a *AuthAPI) Logout(ctx context.Context, _ api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	err := a.svc.Logout(ctx, middleware.CallerIdentity(ctx))
	switch {
	case errors.Is(err, service.ErrRecoveryLogout):
		return api.Logout409JSONResponse{
			Error: "the recovery token is revoked by the reset file, not by logout",
		}, nil
	case errors.Is(err, service.ErrInvalidCredential):
		return api.Logout401JSONResponse{
			Body: api.ErrorResponse{Error: "nothing authenticated this request"},
		}, nil
	case err != nil:
		return api.Logout500JSONResponse{
			Error: internalError(a.logger, "logout", err),
		}, nil
	}

	// The clearing cookie is sent whether or not a session authenticated the
	// request. A bearer caller ignores it, and a browser is left signed out
	// either way.
	return api.Logout200JSONResponse{
		Headers: api.Logout200ResponseHeaders{SetCookie: new(a.clearSessionCookie().String())},
	}, nil
}

// GetOIDC reports how to log in with OIDC.
func (a *AuthAPI) GetOIDC(_ context.Context, _ api.GetOIDCRequestObject) (api.GetOIDCResponseObject, error) {
	if a.oidc == nil {
		return api.GetOIDC404JSONResponse{
			Error: "oidc is not configured",
		}, nil
	}

	return api.GetOIDC200JSONResponse{
		Issuer:   a.oidc.Issuer,
		ClientID: a.oidc.ClientID,
	}, nil
}

// The oidcState type is what the login redirect stores in the state cookie
// and the callback checks.
type oidcState struct {
	// The random value echoed back by the issuer.
	State string `json:"state"`
	// The PKCE code verifier the exchange proves.
	Verifier string `json:"verifier"`
}

// OidcLogin starts the browser OIDC flow.
func (a *AuthAPI) OidcLogin(_ context.Context, _ api.OidcLoginRequestObject) (api.OidcLoginResponseObject, error) {
	if a.oidc == nil || a.oidc.Flow == nil {
		return api.OidcLogin404JSONResponse{
			Error: "the browser oidc flow is not configured",
		}, nil
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return api.OidcLogin500JSONResponse{
			Error: internalError(a.logger, "start login", err),
		}, nil
	}

	state := oidcState{
		State:    hex.EncodeToString(nonce),
		Verifier: oauth2.GenerateVerifier(),
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		return api.OidcLogin500JSONResponse{
			Error: internalError(a.logger, "start login", err),
		}, nil
	}

	cookie := &http.Cookie{
		Name:     "takt_oidc",
		Value:    base64.RawURLEncoding.EncodeToString(encoded),
		Path:     "/api/v1/auth/oidc",
		MaxAge:   int(oidcStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}

	return api.OidcLogin302Response{
		Headers: api.OidcLogin302ResponseHeaders{
			Location:  new(a.oidc.Flow.AuthCodeURL(state.State, oauth2.S256ChallengeOption(state.Verifier))),
			SetCookie: new(cookie.String()),
		},
	}, nil
}

// OidcCallback completes the browser OIDC flow.
func (a *AuthAPI) OidcCallback(ctx context.Context, request api.OidcCallbackRequestObject) (api.OidcCallbackResponseObject, error) {
	if a.oidc == nil || a.oidc.Flow == nil {
		return api.OidcCallback404JSONResponse{
			Error: "the browser oidc flow is not configured",
		}, nil
	}

	refused := func(reason string) api.OidcCallbackResponseObject {
		return api.OidcCallback401JSONResponse{
			Body: api.ErrorResponse{Error: reason},
		}
	}

	if request.Params.Code == nil || request.Params.State == nil || request.Params.TaktOidc == nil {
		return refused("the callback is missing its code, state or cookie"), nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(*request.Params.TaktOidc)
	if err != nil {
		return refused("the state cookie does not decode"), nil
	}

	var state oidcState
	if err = json.Unmarshal(decoded, &state); err != nil {
		return refused("the state cookie does not decode"), nil
	}

	if state.State == "" || state.State != *request.Params.State {
		return refused("the state does not match the one this server issued"), nil
	}

	exchanged, err := a.oidc.Flow.Exchange(ctx, *request.Params.Code, oauth2.VerifierOption(state.Verifier))
	if err != nil {
		return refused("the issuer refused the authorization code"), nil
	}

	rawIDToken, ok := exchanged.Extra("id_token").(string)
	if !ok {
		return refused("the issuer's response carries no identity token"), nil
	}

	token, credential, err := a.svc.LoginOIDC(ctx, rawIDToken)
	switch {
	case errors.Is(err, service.ErrInvalidCredential):
		return refused("invalid identity"), nil
	case err != nil:
		return api.OidcCallback500JSONResponse{
			Error: internalError(a.logger, "complete login", err),
		}, nil
	}

	return api.OidcCallback302Response{
		Headers: api.OidcCallback302ResponseHeaders{
			Location:  new("/"),
			SetCookie: new(a.sessionCookie(credential, token.ExpiresAt).String()),
		},
	}, nil
}

// sessionCookie carries a minted credential to the browser. HttpOnly, so a
// page script can never read it, and Lax, so it survives the top-level
// navigation the OIDC callback ends in.
func (a *AuthAPI) sessionCookie(credential string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    credential,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// clearSessionCookie tells the browser to drop the session cookie.
func (a *AuthAPI) clearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
}
