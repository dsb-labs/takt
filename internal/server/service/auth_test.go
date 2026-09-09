package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func newTestAuthService(t *testing.T, tokens service.TokenRepository, policies service.PolicyReader, verifier service.IdentityVerifier) *service.AuthService {
	t.Helper()

	return service.NewAuthService(service.AuthServiceConfig{
		Logger:   newTestLogger(t),
		Tokens:   tokens,
		Policies: policies,
		Verifier: verifier,
	})
}

// grantAll is a policy granting the viewer role to the test principal.
var grantAll = manifest.Policy{
	Version: "v1",
	Grants: []manifest.PolicyGrant{
		{Principals: []string{"prometheus"}, Role: manifest.RoleViewer},
	},
}

func TestAuthService_Authenticate(t *testing.T) {
	t.Parallel()

	credential, hash, err := auth.NewToken(auth.KindClient)
	require.NoError(t, err)

	t.Run("resolves a client token against the policy", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, hash).Return(database.Token{
			ID: "id", Type: "client", Source: "static", Principal: "prometheus",
		}, nil).Once()
		tokens.EXPECT().Touch(mock.Anything, "id", mock.Anything).Return(nil).Once()

		policies := NewMockPolicyReader(t)
		policies.EXPECT().Get(mock.Anything).Return(grantAll, "tag", nil).Once()

		identity, err := newTestAuthService(t, tokens, policies, nil).Authenticate(t.Context(), credential)
		require.NoError(t, err)
		assert.Equal(t, "prometheus", identity.Principal)
		assert.Equal(t, manifest.RoleViewer, identity.Role)
		assert.Equal(t, "id", identity.TokenID)
		assert.False(t, identity.Recovery)
	})

	t.Run("resolves a principal the policy grants nothing", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, hash).Return(database.Token{
			ID: "id", Type: "client", Source: "static", Principal: "stranger",
		}, nil).Once()
		tokens.EXPECT().Touch(mock.Anything, "id", mock.Anything).Return(nil).Once()

		policies := NewMockPolicyReader(t)
		policies.EXPECT().Get(mock.Anything).Return(grantAll, "tag", nil).Once()

		// Authenticated with no role is a real state: whoami reports it, so a
		// principal can see exactly where onboarding stopped.
		identity, err := newTestAuthService(t, tokens, policies, nil).Authenticate(t.Context(), credential)
		require.NoError(t, err)
		assert.Equal(t, "stranger", identity.Principal)
		assert.Empty(t, identity.Role)
	})

	t.Run("resolves the recovery token above policy", func(t *testing.T) {
		recovery, recoveryHash, err := auth.NewToken(auth.KindRecovery)
		require.NoError(t, err)

		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, recoveryHash).Return(database.Token{
			ID: "id", Type: "recovery", Source: "init",
		}, nil).Once()
		tokens.EXPECT().Touch(mock.Anything, "id", mock.Anything).Return(nil).Once()

		identity, err := newTestAuthService(t, tokens, NewMockPolicyReader(t), nil).Authenticate(t.Context(), recovery)
		require.NoError(t, err)
		assert.True(t, identity.Recovery)
		assert.Equal(t, manifest.RoleAdmin, identity.Role)
	})

	t.Run("refuses a credential that is not a token", func(t *testing.T) {
		svc := newTestAuthService(t, NewMockTokenRepository(t), NewMockPolicyReader(t), nil)

		_, err := svc.Authenticate(t.Context(), "Bearer nonsense")
		assert.ErrorIs(t, err, service.ErrInvalidCredential)
	})

	t.Run("refuses a token the database does not hold", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, hash).Return(database.Token{}, database.ErrTokenNotFound).Once()

		_, err := newTestAuthService(t, tokens, NewMockPolicyReader(t), nil).Authenticate(t.Context(), credential)
		assert.ErrorIs(t, err, service.ErrInvalidCredential)
	})

	t.Run("refuses an expired token", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, hash).Return(database.Token{
			ID: "id", Type: "client", Principal: "prometheus",
			ExpiresAt: time.Now().Add(-time.Minute),
		}, nil).Once()

		_, err := newTestAuthService(t, tokens, NewMockPolicyReader(t), nil).Authenticate(t.Context(), credential)
		assert.ErrorIs(t, err, service.ErrInvalidCredential)
	})

	t.Run("does not record a use within the last minute", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, hash).Return(database.Token{
			ID: "id", Type: "client", Principal: "prometheus",
			LastUsedAt: time.Now().UTC(),
		}, nil).Once()

		policies := NewMockPolicyReader(t)
		policies.EXPECT().Get(mock.Anything).Return(grantAll, "tag", nil).Once()

		// The mock asserts no Touch, since it was never told to expect one.
		_, err := newTestAuthService(t, tokens, policies, nil).Authenticate(t.Context(), credential)
		require.NoError(t, err)
	})
}

func TestAuthService_Logout(t *testing.T) {
	t.Parallel()

	t.Run("revokes the credential that authenticated", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().Delete(mock.Anything, "id").Return(nil).Once()

		svc := newTestAuthService(t, tokens, NewMockPolicyReader(t), nil)
		assert.NoError(t, svc.Logout(t.Context(), auth.Identity{Principal: "ci", TokenID: "id"}))
	})

	t.Run("refuses the recovery token", func(t *testing.T) {
		svc := newTestAuthService(t, NewMockTokenRepository(t), NewMockPolicyReader(t), nil)

		err := svc.Logout(t.Context(), auth.Identity{Recovery: true, TokenID: "id"})
		assert.ErrorIs(t, err, service.ErrRecoveryLogout)
	})

	t.Run("treats an already-revoked credential as revoked", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().Delete(mock.Anything, "id").Return(database.ErrTokenNotFound).Once()

		svc := newTestAuthService(t, tokens, NewMockPolicyReader(t), nil)
		assert.NoError(t, svc.Logout(t.Context(), auth.Identity{Principal: "ci", TokenID: "id"}))
	})
}

func TestAuthService_LoginOIDC(t *testing.T) {
	t.Parallel()

	policy := manifest.Policy{
		Version: "v1",
		OIDC:    &manifest.PolicyOIDC{PrincipalClaim: "email", GroupsClaim: "groups"},
	}

	t.Run("exchanges an identity for a short-lived token", func(t *testing.T) {
		verifier := NewMockIdentityVerifier(t)
		verifier.EXPECT().Verify(mock.Anything, "raw").Return(map[string]any{
			"email":  "david@dsb.dev",
			"groups": []any{"infra"},
		}, nil).Once()

		policies := NewMockPolicyReader(t)
		policies.EXPECT().Get(mock.Anything).Return(policy, "tag", nil).Once()

		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Type == "client" && token.Source == "oidc" &&
				token.Principal == "david@dsb.dev" &&
				len(token.Groups) == 1 && token.Groups[0] == "infra" &&
				!token.ExpiresAt.IsZero()
		})).Return(database.Token{ID: "id", Type: "client", Source: "oidc", Principal: "david@dsb.dev"}, nil).Once()

		minted, credential, err := newTestAuthService(t, tokens, policies, verifier).LoginOIDC(t.Context(), "raw")
		require.NoError(t, err)
		assert.Equal(t, "david@dsb.dev", minted.Principal)

		kind, ok := auth.KindOf(credential)
		assert.True(t, ok)
		assert.Equal(t, auth.KindClient, kind)
	})

	t.Run("refuses an identity without the principal claim", func(t *testing.T) {
		verifier := NewMockIdentityVerifier(t)
		verifier.EXPECT().Verify(mock.Anything, "raw").Return(map[string]any{"sub": "abc"}, nil).Once()

		policies := NewMockPolicyReader(t)
		policies.EXPECT().Get(mock.Anything).Return(policy, "tag", nil).Once()

		_, _, err := newTestAuthService(t, NewMockTokenRepository(t), policies, verifier).LoginOIDC(t.Context(), "raw")
		assert.ErrorIs(t, err, service.ErrInvalidCredential)
	})

	t.Run("refuses an identity the verifier rejects", func(t *testing.T) {
		verifier := NewMockIdentityVerifier(t)
		verifier.EXPECT().Verify(mock.Anything, "raw").Return(nil, assert.AnError).Once()

		_, _, err := newTestAuthService(t, NewMockTokenRepository(t), NewMockPolicyReader(t), verifier).LoginOIDC(t.Context(), "raw")
		assert.ErrorIs(t, err, service.ErrInvalidCredential)
	})

	t.Run("reports oidc is not configured", func(t *testing.T) {
		svc := newTestAuthService(t, NewMockTokenRepository(t), NewMockPolicyReader(t), nil)

		_, _, err := svc.LoginOIDC(t.Context(), "raw")
		assert.ErrorIs(t, err, service.ErrOIDCDisabled)
	})
}

func TestAuthService_LoginToken(t *testing.T) {
	t.Parallel()

	credential, hash, err := auth.NewToken(auth.KindClient)
	require.NoError(t, err)

	t.Run("exchanges a client token for a session token", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, hash).Return(database.Token{
			ID: "id", Type: "client", Source: "static", Principal: "prometheus",
		}, nil).Once()
		tokens.EXPECT().Touch(mock.Anything, "id", mock.Anything).Return(nil).Once()
		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Source == "session" && token.Principal == "prometheus" && !token.ExpiresAt.IsZero()
		})).Return(database.Token{ID: "session-id", Type: "client", Source: "session", Principal: "prometheus"}, nil).Once()

		policies := NewMockPolicyReader(t)
		policies.EXPECT().Get(mock.Anything).Return(grantAll, "tag", nil).Once()

		minted, session, err := newTestAuthService(t, tokens, policies, nil).LoginToken(t.Context(), credential)
		require.NoError(t, err)
		assert.Equal(t, "session-id", minted.ID)
		assert.NotEqual(t, credential, session)
	})

	t.Run("refuses the recovery token", func(t *testing.T) {
		recovery, recoveryHash, err := auth.NewToken(auth.KindRecovery)
		require.NoError(t, err)

		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().GetByHash(mock.Anything, recoveryHash).Return(database.Token{
			ID: "id", Type: "recovery", Source: "init",
		}, nil).Once()
		tokens.EXPECT().Touch(mock.Anything, "id", mock.Anything).Return(nil).Once()

		_, _, err = newTestAuthService(t, tokens, NewMockPolicyReader(t), nil).LoginToken(t.Context(), recovery)
		assert.ErrorIs(t, err, service.ErrInvalidCredential)
	})
}
