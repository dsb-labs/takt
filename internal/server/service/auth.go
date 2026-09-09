package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/pkg/manifest"
)

var (
	// ErrInvalidCredential is returned when a presented credential does not
	// authenticate. It does not distinguish between the reasons it might not:
	// a caller that could tell an unknown token from an expired one would be
	// told something about what exists.
	ErrInvalidCredential = errors.New("invalid credential")
	// ErrRecoveryLogout is returned when the recovery token asks to revoke
	// itself. Its revocation path is deliberately host-level: the reset file.
	ErrRecoveryLogout = errors.New("the recovery token is revoked by the reset file, not by logout")
	// ErrOIDCDisabled is returned when a login is asked for and the server
	// carries no OIDC configuration.
	ErrOIDCDisabled = errors.New("oidc is not configured")
)

// The claim a principal is read from when the policy does not name one.
// Email by convention, so a human principal cannot collide with a machine's
// bare name.
const defaultPrincipalClaim = "email"

type (
	// The PolicyReader interface describes how the auth service reads the
	// policy a client token is evaluated against.
	PolicyReader interface {
		// Get should return the current policy and its tag.
		Get(ctx context.Context) (manifest.Policy, string, error)
	}

	// The IdentityVerifier interface describes how a raw OIDC identity token
	// is checked and read.
	//
	// Declared here rather than taken as the concrete type so that the
	// service can be tested without an identity provider.
	IdentityVerifier interface {
		// Verify should check the raw identity token against the issuer and
		// return its claims.
		Verify(ctx context.Context, rawIDToken string) (map[string]any, error)
	}

	// The AuthService type resolves credentials to identities and mints the
	// tokens a login produces.
	AuthService struct {
		logger   *slog.Logger
		tokens   TokenRepository
		policies PolicyReader
		verifier IdentityVerifier
	}

	// The AuthServiceConfig type contains fields used to construct an
	// AuthService.
	AuthServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the tokens.
		Tokens TokenRepository
		// The reader for the policy a client token is evaluated against.
		Policies PolicyReader
		// The verifier for OIDC identity tokens. May be nil, in which case
		// logins report ErrOIDCDisabled and static tokens are the only
		// authentication.
		Verifier IdentityVerifier
	}
)

// NewAuthService returns a new instance of the AuthService type.
func NewAuthService(config AuthServiceConfig) *AuthService {
	return &AuthService{
		logger:   config.Logger.With("component", "service"),
		tokens:   config.Tokens,
		policies: config.Policies,
		verifier: config.Verifier,
	}
}

// Authenticate resolves a presented credential to the identity it proves,
// evaluating a client token against the policy so a grant change applies to
// the very next request. Reports ErrInvalidCredential for anything that does
// not authenticate.
func (s *AuthService) Authenticate(ctx context.Context, credential string) (auth.Identity, error) {
	if _, ok := auth.KindOf(credential); !ok {
		return auth.Identity{}, ErrInvalidCredential
	}

	token, err := s.tokens.GetByHash(ctx, auth.HashToken(credential))
	switch {
	case errors.Is(err, database.ErrTokenNotFound):
		return auth.Identity{}, ErrInvalidCredential
	case err != nil:
		return auth.Identity{}, err
	}

	if !token.ExpiresAt.IsZero() && time.Now().After(token.ExpiresAt) {
		return auth.Identity{}, ErrInvalidCredential
	}

	// Recorded at most once a minute, so the audit answer stays fresh without
	// a scraper's every request becoming a write. A failure costs the
	// freshness of last-used, not the request.
	if now := time.Now().UTC(); token.LastUsedAt.IsZero() || now.Sub(token.LastUsedAt) > time.Minute {
		if err = s.tokens.Touch(ctx, token.ID, now); err != nil {
			s.logger.With("error", err, "token_id", token.ID).Warn("failed to record token use")
		}
	}

	if token.Type == string(auth.KindRecovery) {
		return auth.Identity{Role: manifest.RoleAdmin, TokenID: token.ID, Recovery: true}, nil
	}

	policy, _, err := s.policies.Get(ctx)
	if err != nil {
		return auth.Identity{}, err
	}

	role, _ := auth.Evaluate(policy, token.Principal, token.Groups)

	return auth.Identity{
		Principal: token.Principal,
		Groups:    token.Groups,
		Role:      role,
		TokenID:   token.ID,
	}, nil
}

// Logout revokes the credential that authenticated the request, whatever its
// kind. Self-revocation is always safe, with one refusal: the recovery
// token, whose revocation path is deliberately host-level.
func (s *AuthService) Logout(ctx context.Context, identity auth.Identity) error {
	if identity.Recovery {
		return ErrRecoveryLogout
	}

	if identity.TokenID == "" {
		return ErrInvalidCredential
	}

	err := s.tokens.Delete(ctx, identity.TokenID)
	switch {
	case errors.Is(err, database.ErrTokenNotFound):
		return nil
	case err != nil:
		return err
	}

	s.logger.With("principal", identity.Principal).Info("credential revoked by logout")

	return nil
}

// LoginOIDC exchanges a verified OIDC identity for a short-lived client
// token bound to the principal the policy's claim mapping derives. OIDC
// never becomes a parallel authorization path: identity in, token out, and
// the policy only ever sees tokens.
func (s *AuthService) LoginOIDC(ctx context.Context, rawIDToken string) (Token, string, error) {
	if s.verifier == nil {
		return Token{}, "", ErrOIDCDisabled
	}

	claims, err := s.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Token{}, "", fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}

	policy, _, err := s.policies.Get(ctx)
	if err != nil {
		return Token{}, "", err
	}

	principalClaim := defaultPrincipalClaim

	var groupsClaim string
	if policy.OIDC != nil {
		principalClaim = policy.OIDC.PrincipalClaim
		groupsClaim = policy.OIDC.GroupsClaim
	}

	principal, ok := claims[principalClaim].(string)
	if !ok || principal == "" {
		return Token{}, "", fmt.Errorf("%w: the identity carries no %q claim", ErrInvalidCredential, principalClaim)
	}

	return s.mint(ctx, "oidc", principal, claimedGroups(claims, groupsClaim))
}

// LoginToken exchanges an existing client token for a short-lived session
// token bound to the same principal and groups. This is how the UI holds a
// credential that expires on its own rather than the standing one that was
// pasted into it.
func (s *AuthService) LoginToken(ctx context.Context, credential string) (Token, string, error) {
	identity, err := s.Authenticate(ctx, credential)
	if err != nil {
		return Token{}, "", err
	}

	// A recovery token stays out of sessions for the same reason it is not
	// for daily use: everything it touches bypasses policy.
	if identity.Recovery {
		return Token{}, "", ErrInvalidCredential
	}

	return s.mint(ctx, "session", identity.Principal, identity.Groups)
}

// mint stores a short-lived client token and returns it with its credential.
func (s *AuthService) mint(ctx context.Context, source, principal string, groups []string) (Token, string, error) {
	credential, hash, err := auth.NewToken(auth.KindClient)
	if err != nil {
		return Token{}, "", err
	}

	stored, err := s.tokens.Create(ctx, database.Token{
		Hash:      hash,
		Type:      string(auth.KindClient),
		Source:    source,
		Principal: principal,
		Groups:    groups,
		ExpiresAt: time.Now().UTC().Add(auth.TokenTTL),
	})
	if err != nil {
		return Token{}, "", err
	}

	s.logger.With("principal", principal, "source", source).Info("login minted a token")

	return newToken(stored), credential, nil
}

// claimedGroups reads the groups claim as the list of names it holds. A
// missing or misshapen claim reads as no groups rather than an error: groups
// only ever widen what a grant could match, so their absence is safe.
func claimedGroups(claims map[string]any, groupsClaim string) []string {
	if groupsClaim == "" {
		return nil
	}

	values, ok := claims[groupsClaim].([]any)
	if !ok {
		return nil
	}

	groups := make([]string, 0, len(values))
	for _, value := range values {
		if group, ok := value.(string); ok {
			groups = append(groups, group)
		}
	}

	return groups
}

// The oidcVerifier type adapts go-oidc's discovery and verification to the
// IdentityVerifier interface.
type oidcVerifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewOIDCVerifier discovers the issuer's keys and returns a verifier for
// identity tokens minted for the given client.
func NewOIDCVerifier(ctx context.Context, issuer, clientID string) (IdentityVerifier, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("failed to discover the oidc issuer: %w", err)
	}

	return &oidcVerifier{verifier: provider.Verifier(&oidc.Config{ClientID: clientID})}, nil
}

// Verify checks the raw identity token against the issuer and returns its
// claims.
func (v *oidcVerifier) Verify(ctx context.Context, rawIDToken string) (map[string]any, error) {
	idToken, err := v.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("failed to verify the identity token: %w", err)
	}

	var claims map[string]any
	if err = idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("failed to read the identity token's claims: %w", err)
	}

	return claims, nil
}
