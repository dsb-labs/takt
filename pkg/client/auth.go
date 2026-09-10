package client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
)

type (
	// The Identity type is the client-side view of who the caller is.
	Identity struct {
		// Whether authentication is enabled on the server.
		Enabled bool
		// The name the caller authenticated as. "anonymous" when
		// authentication is disabled, and empty for the recovery token.
		Principal string
		// The role the policy grants the caller, empty when it grants
		// nothing.
		Role string
		// The groups asserted for the caller when its token was minted.
		Groups []string
		// Whether the caller authenticated with the recovery token.
		Recovery bool
	}

	// The Login type is the client-side view of a successful login: the
	// short-lived credential a verified identity was exchanged for.
	Login struct {
		// The minted token. Reported this once and never again.
		Credential string
		// The principal the token is bound to.
		Principal string
		// The time the token stops authenticating.
		ExpiresAt time.Time
	}

	// The OIDC type is the client-side view of the server's OIDC
	// configuration, which is what a login needs to run the authorization
	// code flow.
	OIDC struct {
		// The issuer the flow runs against.
		Issuer string
		// The client identifier registered with the issuer.
		ClientID string
	}
)

// WhoAmI returns the caller's principal, role and groups. It needs
// authentication but no role, so a principal holding a token with no grants
// yet sees exactly that state.
func (c *Client) WhoAmI(ctx context.Context) (Identity, error) {
	resp, err := c.api.GetAuthWithResponse(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return Identity{
			Enabled:   resp.JSON200.Enabled,
			Principal: resp.JSON200.Principal,
			Role:      resp.JSON200.Role,
			Groups:    resp.JSON200.Groups,
			Recovery:  resp.JSON200.Recovery,
		}, nil
	case resp.JSON500 != nil:
		return Identity{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Identity{}, newError(resp.StatusCode(), nil)
	}
}

// Login exchanges a verified OIDC identity token for a short-lived client
// token, reporting ErrOIDCNotConfigured when the server has no issuer to
// verify it against.
func (c *Client) Login(ctx context.Context, idToken string) (Login, error) {
	resp, err := c.api.LoginWithResponse(ctx, api.LoginRequest{IDToken: new(idToken)})
	if err != nil {
		return Login{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return Login{
			Credential: resp.JSON200.Credential,
			Principal:  resp.JSON200.Principal,
			ExpiresAt:  resp.JSON200.ExpiresAt,
		}, nil
	case resp.JSON400 != nil:
		return Login{}, fmt.Errorf("%s: %w", resp.JSON400.Error, ErrOIDCNotConfigured)
	case resp.JSON401 != nil:
		return Login{}, newError(http.StatusUnauthorized, resp.JSON401)
	case resp.JSON500 != nil:
		return Login{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Login{}, newError(resp.StatusCode(), nil)
	}
}

// LoginCode trades an authorization code from a loopback flow for a
// short-lived client token. The server performs the exchange with the
// issuer, because the exchange is what needs the client secret and the
// secret never reaches a client.
func (c *Client) LoginCode(ctx context.Context, code, verifier, redirectURI string) (Login, error) {
	resp, err := c.api.LoginWithResponse(ctx, api.LoginRequest{
		Code:        new(code),
		Verifier:    new(verifier),
		RedirectURI: new(redirectURI),
	})
	if err != nil {
		return Login{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return Login{
			Credential: resp.JSON200.Credential,
			Principal:  resp.JSON200.Principal,
			ExpiresAt:  resp.JSON200.ExpiresAt,
		}, nil
	case resp.JSON400 != nil:
		return Login{}, newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON401 != nil:
		return Login{}, newError(http.StatusUnauthorized, resp.JSON401)
	case resp.JSON500 != nil:
		return Login{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Login{}, newError(resp.StatusCode(), nil)
	}
}

// Logout revokes the credential this client authenticated with, whatever its
// kind. The one refusal is the recovery token, reported as
// ErrRecoveryLogout: its revocation path is the reset file, not this call.
func (c *Client) Logout(ctx context.Context) error {
	resp, err := c.api.LogoutWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON409 != nil:
		return fmt.Errorf("%s: %w", resp.JSON409.Error, ErrRecoveryLogout)
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

// GetOIDC returns the issuer and client identifier a login runs the
// authorization code flow against, reporting ErrOIDCNotConfigured when the
// server carries no OIDC configuration.
func (c *Client) GetOIDC(ctx context.Context) (OIDC, error) {
	resp, err := c.api.GetOIDCWithResponse(ctx)
	if err != nil {
		return OIDC{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return OIDC{Issuer: resp.JSON200.Issuer, ClientID: resp.JSON200.ClientID}, nil
	case resp.JSON404 != nil:
		return OIDC{}, fmt.Errorf("%s: %w", resp.JSON404.Error, ErrOIDCNotConfigured)
	case resp.JSON500 != nil:
		return OIDC{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return OIDC{}, newError(resp.StatusCode(), nil)
	}
}
