package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/service"
)

type (
	// The TokenService interface describes the token operations the API
	// exposes.
	TokenService interface {
		// Create should mint a client token bound to the given principal,
		// returning the stored token and the credential itself.
		Create(ctx context.Context, principal string) (service.Token, string, error)
		// List should return every token, newest first.
		List(ctx context.Context) ([]service.Token, error)
		// Delete should revoke the token with the given identifier.
		Delete(ctx context.Context, id string) error
	}

	// The TokenAPI type exposes HTTP endpoints for managing tokens.
	TokenAPI struct {
		logger *slog.Logger
		tokens TokenService
	}

	// The TokenAPIConfig type contains fields used to construct a TokenAPI.
	TokenAPIConfig struct {
		// The logger used to record failures the response deliberately
		// doesn't describe.
		Logger *slog.Logger
		// The service performing the token operations.
		Tokens TokenService
	}
)

// NewTokenAPI returns a new instance of the TokenAPI type.
func NewTokenAPI(config TokenAPIConfig) *TokenAPI {
	return &TokenAPI{
		logger: config.Logger.With("component", "api"),
		tokens: config.Tokens,
	}
}

// CreateToken mints a client token bound to a principal.
func (a *TokenAPI) CreateToken(ctx context.Context, request api.CreateTokenRequestObject) (api.CreateTokenResponseObject, error) {
	if request.Body == nil {
		return api.CreateToken400JSONResponse{
			Error: "request body is required",
		}, nil
	}

	token, credential, err := a.tokens.Create(ctx, request.Body.Principal)
	switch {
	case errors.Is(err, service.ErrInvalidPrincipal):
		return api.CreateToken400JSONResponse{
			Error: err.Error(),
		}, nil
	case err != nil:
		return api.CreateToken500JSONResponse{
			Error: internalError(a.logger, "create token", err),
		}, nil
	}

	return api.CreateToken201JSONResponse{
		Credential: credential,
		Token:      newToken(token),
	}, nil
}

// ListTokens names every credential the server holds.
func (a *TokenAPI) ListTokens(ctx context.Context, _ api.ListTokensRequestObject) (api.ListTokensResponseObject, error) {
	tokens, err := a.tokens.List(ctx)
	if err != nil {
		return api.ListTokens500JSONResponse{
			Error: internalError(a.logger, "list tokens", err),
		}, nil
	}

	out := make([]api.Token, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, newToken(token))
	}

	return api.ListTokens200JSONResponse{Tokens: out}, nil
}

// DeleteToken revokes the token with the given identifier.
func (a *TokenAPI) DeleteToken(ctx context.Context, request api.DeleteTokenRequestObject) (api.DeleteTokenResponseObject, error) {
	err := a.tokens.Delete(ctx, request.ID)
	switch {
	case errors.Is(err, service.ErrTokenNotFound):
		return api.DeleteToken404JSONResponse{
			Error: fmt.Sprintf("token %q does not exist", request.ID),
		}, nil
	case err != nil:
		return api.DeleteToken500JSONResponse{
			Error: internalError(a.logger, "delete token", err),
		}, nil
	}

	return api.DeleteToken200JSONResponse{}, nil
}

// newToken maps a token record to its wire shape. Optional times are sent as
// absent rather than as zero values.
func newToken(token service.Token) api.Token {
	out := api.Token{
		ID:        token.ID,
		Type:      api.TokenType(token.Type),
		Source:    api.TokenSource(token.Source),
		Principal: token.Principal,
		CreatedAt: token.CreatedAt,
	}

	if !token.ExpiresAt.IsZero() {
		out.ExpiresAt = new(token.ExpiresAt)
	}
	if !token.LastUsedAt.IsZero() {
		out.LastUsedAt = new(token.LastUsedAt)
	}

	return out
}
