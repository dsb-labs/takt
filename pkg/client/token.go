package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
)

// The Token type is the client-side view of a credential's record. The
// credential itself is reported once, by the create that minted it, and never
// again.
type Token struct {
	// The identifier the server assigns, which a delete names.
	ID string
	// Whether this is the recovery token or a client token.
	Type string
	// What minted the token: "init", "static", "oidc" or "session".
	Source string
	// The principal the token authenticates as. Empty for the recovery
	// token.
	Principal string
	// The time the token stops authenticating. Zero when it does not
	// expire.
	ExpiresAt time.Time
	// The time the token was created.
	CreatedAt time.Time
	// The time the token last authenticated a request. Zero when it never
	// has.
	LastUsedAt time.Time
}

// checkTokenID reports whether an identifier is usable as a single segment of
// a request path, for the same reason a volume's name is checked.
func checkTokenID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%w: %q is not a single path segment", ErrInvalidTokenID, id)
	}

	return nil
}

// CreateToken mints a static client token bound to the given principal,
// returning its record and the credential itself. The credential is reported
// this once: the server stores only a hash of it.
func (c *Client) CreateToken(ctx context.Context, principal string) (Token, string, error) {
	resp, err := c.api.CreateTokenWithResponse(ctx, api.CreateTokenRequest{Principal: principal})
	if err != nil {
		return Token{}, "", fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON201 != nil:
		return newToken(resp.JSON201.Token), resp.JSON201.Credential, nil
	case resp.JSON400 != nil:
		return Token{}, "", newError(http.StatusBadRequest, resp.JSON400)
	case resp.JSON500 != nil:
		return Token{}, "", newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Token{}, "", newError(resp.StatusCode(), nil)
	}
}

// ListTokens names every credential the server holds, newest first. With
// GetPolicy, this answers "who can touch this server" completely.
func (c *Client) ListTokens(ctx context.Context) ([]Token, error) {
	resp, err := c.api.ListTokensWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		tokens := make([]Token, 0, len(resp.JSON200.Tokens))
		for _, token := range resp.JSON200.Tokens {
			tokens = append(tokens, newToken(token))
		}

		return tokens, nil
	case resp.JSON500 != nil:
		return nil, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return nil, newError(resp.StatusCode(), nil)
	}
}

// DeleteToken revokes the token with the given identifier, returning
// ErrTokenNotFound when no such token exists. Revocation is immediate: the
// next request presenting the credential is refused.
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	if err := checkTokenID(id); err != nil {
		return err
	}

	resp, err := c.api.DeleteTokenWithResponse(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return nil
	case resp.JSON404 != nil:
		return fmt.Errorf("%s: %w", resp.JSON404.Error, ErrTokenNotFound)
	case resp.JSON500 != nil:
		return newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return newError(resp.StatusCode(), nil)
	}
}

// newToken maps a wire token record onto the canonical shape. Absent optional
// times read as zero values.
func newToken(token api.Token) Token {
	out := Token{
		ID:        token.ID,
		Type:      string(token.Type),
		Source:    string(token.Source),
		Principal: token.Principal,
		CreatedAt: token.CreatedAt,
	}

	if token.ExpiresAt != nil {
		out.ExpiresAt = *token.ExpiresAt
	}
	if token.LastUsedAt != nil {
		out.LastUsedAt = *token.LastUsedAt
	}

	return out
}
