package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/database"
)

var (
	// ErrTokenNotFound is returned when the requested token does not exist.
	ErrTokenNotFound = errors.New("token not found")
	// ErrACLInitialized is returned when init is asked for and a recovery
	// token already exists.
	ErrACLInitialized = errors.New("acl already initialized")
	// ErrInvalidPrincipal is returned when a token is created for a principal
	// name takt will not accept.
	ErrInvalidPrincipal = errors.New("invalid principal")
)

// The longest principal name a token binds to. Generous enough for any email
// address, small enough to display and store without thought.
const maxPrincipalLength = 256

type (
	// The TokenRepository interface describes the persistence operations the
	// token and auth services use.
	TokenRepository interface {
		// Create should record the token and return it with its assigned
		// identifier and creation time.
		Create(ctx context.Context, token database.Token) (database.Token, error)
		// GetByHash should return the token a presented credential hashes to.
		GetByHash(ctx context.Context, hash string) (database.Token, error)
		// List should return every token, newest first.
		List(ctx context.Context) ([]database.Token, error)
		// Delete should remove the token with the given identifier.
		Delete(ctx context.Context, id string) error
		// DeleteRecovery should remove the recovery token if one exists.
		DeleteRecovery(ctx context.Context) error
		// Touch should record that the token authenticated a request at the
		// given time.
		Touch(ctx context.Context, id string, at time.Time) error
		// DeleteExpired should remove every token whose expiry has passed,
		// returning how many were removed.
		DeleteExpired(ctx context.Context, now time.Time) (int64, error)
	}

	// The Token type describes a credential as it is reported to a caller.
	// The credential itself is reported once, by the create that minted it,
	// and never again.
	Token struct {
		// The identifier the server assigns to the token.
		ID string
		// Whether the token is the recovery token or a client token.
		Type string
		// What minted the token: "init", "static", "oidc" or "session".
		Source string
		// The principal the token authenticates as. Empty for the recovery
		// token.
		Principal string
		// The time after which the token no longer authenticates. Zero when
		// the token does not expire.
		ExpiresAt time.Time
		// The time the token was created.
		CreatedAt time.Time
		// The time the token last authenticated a request. Zero when it never
		// has.
		LastUsedAt time.Time
	}

	// The TokenService type owns the credentials that authenticate API
	// callers.
	TokenService struct {
		logger *slog.Logger
		tokens TokenRepository
	}

	// The TokenServiceConfig type contains fields used to construct a
	// TokenService.
	TokenServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the tokens.
		Tokens TokenRepository
	}
)

// NewTokenService returns a new instance of the TokenService type.
func NewTokenService(config TokenServiceConfig) *TokenService {
	return &TokenService{
		logger: config.Logger.With("component", "service"),
		tokens: config.Tokens,
	}
}

// Init mints the recovery token, returning the credential itself. It works
// exactly once: a second init reports ErrACLInitialized, and only writing the
// reset file into the data directory makes it work again.
func (s *TokenService) Init(ctx context.Context) (string, error) {
	credential, hash, err := auth.NewToken(auth.KindRecovery)
	if err != nil {
		return "", err
	}

	_, err = s.tokens.Create(ctx, database.Token{
		Hash:   hash,
		Type:   string(auth.KindRecovery),
		Source: "init",
	})
	switch {
	case errors.Is(err, database.ErrTokenAlreadyExists):
		return "", ErrACLInitialized
	case err != nil:
		return "", err
	}

	s.logger.Info("acl initialized")

	return credential, nil
}

// Reset removes the recovery token if one exists, which is what consuming the
// reset file asks for. Client tokens and the policy survive: the reset exists
// to recover from a lost recovery token, not to start over.
func (s *TokenService) Reset(ctx context.Context) error {
	return s.tokens.DeleteRecovery(ctx)
}

// Create mints a client token bound to the given principal, returning the
// stored token and the credential itself. This is the only time the
// credential is reported.
func (s *TokenService) Create(ctx context.Context, principal string) (Token, string, error) {
	if err := validatePrincipal(principal); err != nil {
		return Token{}, "", err
	}

	credential, hash, err := auth.NewToken(auth.KindClient)
	if err != nil {
		return Token{}, "", err
	}

	stored, err := s.tokens.Create(ctx, database.Token{
		Hash:      hash,
		Type:      string(auth.KindClient),
		Source:    "static",
		Principal: principal,
	})
	if err != nil {
		return Token{}, "", err
	}

	s.logger.With("principal", principal).Info("token created")

	return newToken(stored), credential, nil
}

// List returns every token the server records, newest first. With the policy,
// this answers who can touch the server.
func (s *TokenService) List(ctx context.Context) ([]Token, error) {
	stored, err := s.tokens.List(ctx)
	if err != nil {
		return nil, err
	}

	tokens := make([]Token, 0, len(stored))
	for _, token := range stored {
		tokens = append(tokens, newToken(token))
	}

	return tokens, nil
}

// Delete revokes the token with the given identifier. Revocation is
// immediate: the next request presenting the credential is refused.
func (s *TokenService) Delete(ctx context.Context, id string) error {
	err := s.tokens.Delete(ctx, id)
	switch {
	case errors.Is(err, database.ErrTokenNotFound):
		return ErrTokenNotFound
	case err != nil:
		return err
	}

	s.logger.With("token_id", id).Info("token deleted")

	return nil
}

// Sweep removes tokens whose expiry has passed. An expired token already
// refuses to authenticate, so this is hygiene for the token list rather than
// security.
func (s *TokenService) Sweep(ctx context.Context) error {
	deleted, err := s.tokens.DeleteExpired(ctx, time.Now().UTC())
	if err != nil {
		return err
	}

	if deleted > 0 {
		s.logger.With("deleted", deleted).Debug("swept expired tokens")
	}

	return nil
}

// newToken maps a stored token to the shape reported to a caller, which
// carries no hash.
func newToken(token database.Token) Token {
	return Token{
		ID:         token.ID,
		Type:       token.Type,
		Source:     token.Source,
		Principal:  token.Principal,
		ExpiresAt:  token.ExpiresAt,
		CreatedAt:  token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
	}
}

// validatePrincipal reports whether a principal is a name a grant could
// reference. By convention humans are emails and machines are bare names, so
// most printable text passes, but whitespace and control characters would
// only ever be a paste gone wrong.
func validatePrincipal(principal string) error {
	if principal == "" {
		return fmt.Errorf("%w: a principal is required", ErrInvalidPrincipal)
	}

	if len(principal) > maxPrincipalLength {
		return fmt.Errorf("%w: at most %d characters", ErrInvalidPrincipal, maxPrincipalLength)
	}

	if strings.ContainsFunc(principal, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("%w: whitespace and control characters are not allowed", ErrInvalidPrincipal)
	}

	if strings.HasPrefix(principal, "group:") {
		return fmt.Errorf("%w: the group: prefix is how a grant references a group", ErrInvalidPrincipal)
	}

	return nil
}
