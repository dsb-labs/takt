package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/service"
)

func newTestTokenService(t *testing.T, tokens service.TokenRepository) *service.TokenService {
	t.Helper()

	return service.NewTokenService(service.TokenServiceConfig{
		Logger: newTestLogger(t),
		Tokens: tokens,
	})
}

func TestTokenService_Init(t *testing.T) {
	t.Parallel()

	t.Run("mints the recovery token", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Type == "recovery" && token.Source == "init" && token.Principal == ""
		})).Return(database.Token{ID: "id"}, nil).Once()

		credential, err := newTestTokenService(t, tokens).Init(t.Context())
		require.NoError(t, err)

		kind, ok := auth.KindOf(credential)
		assert.True(t, ok)
		assert.Equal(t, auth.KindRecovery, kind)
	})

	t.Run("reports init has already run", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().Create(mock.Anything, mock.Anything).
			Return(database.Token{}, database.ErrTokenAlreadyExists).Once()

		_, err := newTestTokenService(t, tokens).Init(t.Context())
		assert.ErrorIs(t, err, service.ErrACLInitialized)
	})
}

func TestTokenService_Create(t *testing.T) {
	t.Parallel()

	t.Run("mints a client token bound to the principal", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Type == "client" && token.Source == "static" && token.Principal == "prometheus"
		})).Return(database.Token{ID: "id", Type: "client", Source: "static", Principal: "prometheus"}, nil).Once()

		stored, credential, err := newTestTokenService(t, tokens).Create(t.Context(), "prometheus")
		require.NoError(t, err)
		assert.Equal(t, "prometheus", stored.Principal)

		kind, ok := auth.KindOf(credential)
		assert.True(t, ok)
		assert.Equal(t, auth.KindClient, kind)
	})

	t.Run("refuses a principal takt will not accept", func(t *testing.T) {
		tt := []struct {
			Name      string
			Principal string
		}{
			{Name: "empty", Principal: ""},
			{Name: "whitespace", Principal: "two words"},
			{Name: "control character", Principal: "line\nbreak"},
			{Name: "the group prefix", Principal: "group:infra"},
			{Name: "too long", Principal: strings.Repeat("a", 257)},
		}

		for _, tc := range tt {
			t.Run(tc.Name, func(t *testing.T) {
				_, _, err := newTestTokenService(t, NewMockTokenRepository(t)).Create(t.Context(), tc.Principal)
				assert.ErrorIs(t, err, service.ErrInvalidPrincipal)
			})
		}
	})
}

func TestTokenService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("deletes a token", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().Delete(mock.Anything, "id").Return(nil).Once()

		assert.NoError(t, newTestTokenService(t, tokens).Delete(t.Context(), "id"))
	})

	t.Run("reports a token that does not exist", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().Delete(mock.Anything, "id").Return(database.ErrTokenNotFound).Once()

		assert.ErrorIs(t, newTestTokenService(t, tokens).Delete(t.Context(), "id"), service.ErrTokenNotFound)
	})
}

func TestTokenService_List(t *testing.T) {
	t.Parallel()

	tokens := NewMockTokenRepository(t)
	tokens.EXPECT().List(mock.Anything).Return([]database.Token{
		{ID: "one", Hash: "secret-hash", Type: "client", Source: "static", Principal: "ci"},
	}, nil).Once()

	listed, err := newTestTokenService(t, tokens).List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "one", listed[0].ID)
	assert.Equal(t, "ci", listed[0].Principal)
}

func TestTokenService_Sweep(t *testing.T) {
	t.Parallel()

	tokens := NewMockTokenRepository(t)
	tokens.EXPECT().DeleteExpired(mock.Anything, mock.MatchedBy(func(now time.Time) bool {
		return !now.IsZero()
	})).Return(2, nil).Once()

	assert.NoError(t, newTestTokenService(t, tokens).Sweep(t.Context()))
}

func TestTokenService_MintWorkloadToken(t *testing.T) {
	t.Parallel()

	t.Run("mints an expiring shared token, replacing its predecessor", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().DeleteMinted(mock.Anything, "workload-id", "prometheus", (*int)(nil),
			mock.MatchedBy(func(version *int) bool { return version != nil && *version == 3 })).
			Return(nil).Once()
		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Type == "client" && token.Source == "workload" &&
				token.Principal == "prometheus" && token.WorkloadID == "workload-id" &&
				token.WorkloadInstance == nil &&
				token.WorkloadVersion != nil && *token.WorkloadVersion == 3 &&
				time.Until(token.ExpiresAt) > 0
		})).Return(database.Token{ID: "id"}, nil).Once()

		credential, err := newTestTokenService(t, tokens).
			MintWorkloadToken(t.Context(), "prometheus", "workload-id", 3, true)
		require.NoError(t, err)

		kind, ok := auth.KindOf(credential)
		assert.True(t, ok)
		assert.Equal(t, auth.KindClient, kind)
	})

	t.Run("mints without an expiry when nothing can rotate it", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().DeleteMinted(mock.Anything, "workload-id", "prometheus", (*int)(nil), mock.Anything).
			Return(nil).Once()
		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.ExpiresAt.IsZero()
		})).Return(database.Token{ID: "id"}, nil).Once()

		_, err := newTestTokenService(t, tokens).
			MintWorkloadToken(t.Context(), "prometheus", "workload-id", 3, false)
		require.NoError(t, err)
	})

	t.Run("refuses a principal takt will not accept", func(t *testing.T) {
		_, err := newTestTokenService(t, NewMockTokenRepository(t)).
			MintWorkloadToken(t.Context(), "two words", "workload-id", 1, true)
		assert.ErrorIs(t, err, service.ErrInvalidPrincipal)
	})
}

func TestTokenService_MintInstanceToken(t *testing.T) {
	t.Parallel()

	t.Run("mints a token bound to the instance, replacing its predecessor", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().DeleteMinted(mock.Anything, "workload-id", "ci",
			mock.MatchedBy(func(instance *int) bool { return instance != nil && *instance == 1 }),
			(*int)(nil)).
			Return(nil).Once()
		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Type == "client" && token.Source == "workload" &&
				token.Principal == "ci" && token.WorkloadID == "workload-id" &&
				token.WorkloadInstance != nil && *token.WorkloadInstance == 1 &&
				token.WorkloadVersion == nil &&
				// The environment is fixed at process start, so nothing could
				// deliver a renewal and the token must not expire under the
				// instance.
				token.ExpiresAt.IsZero()
		})).Return(database.Token{ID: "id"}, nil).Once()

		credential, err := newTestTokenService(t, tokens).
			MintInstanceToken(t.Context(), "ci", "workload-id", 1)
		require.NoError(t, err)

		kind, ok := auth.KindOf(credential)
		assert.True(t, ok)
		assert.Equal(t, auth.KindClient, kind)
	})

	t.Run("refuses a principal takt will not accept", func(t *testing.T) {
		_, err := newTestTokenService(t, NewMockTokenRepository(t)).
			MintInstanceToken(t.Context(), "", "workload-id", 0)
		assert.ErrorIs(t, err, service.ErrInvalidPrincipal)
	})
}

func TestTokenService_RefreshWorkloadToken(t *testing.T) {
	t.Parallel()

	t.Run("leaves a token with most of its life alone", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().GetForWorkload(mock.Anything, "workload-id", 3, "prometheus").
			Return(database.Token{ExpiresAt: time.Now().UTC().Add(11 * time.Hour)}, nil).Once()

		credential, refreshed, err := newTestTokenService(t, tokens).
			RefreshWorkloadToken(t.Context(), "prometheus", "workload-id", 3, true)
		require.NoError(t, err)
		assert.False(t, refreshed)
		assert.Empty(t, credential)
	})

	t.Run("re-mints a token inside the margin", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().GetForWorkload(mock.Anything, "workload-id", 3, "prometheus").
			Return(database.Token{ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil).Once()
		tokens.EXPECT().DeleteMinted(mock.Anything, "workload-id", "prometheus", (*int)(nil), mock.Anything).
			Return(nil).Once()
		tokens.EXPECT().Create(mock.Anything, mock.MatchedBy(func(token database.Token) bool {
			return token.Source == "workload" && !token.ExpiresAt.IsZero()
		})).Return(database.Token{ID: "id"}, nil).Once()

		credential, refreshed, err := newTestTokenService(t, tokens).
			RefreshWorkloadToken(t.Context(), "prometheus", "workload-id", 3, true)
		require.NoError(t, err)
		assert.True(t, refreshed)
		assert.NotEmpty(t, credential)
	})

	t.Run("re-mints a token that is gone", func(t *testing.T) {
		// A token revoked by hand comes back on the next pass: the manifest
		// asks for a state, and the reconciler re-asserts it.
		tokens := NewMockTokenRepository(t)

		tokens.EXPECT().GetForWorkload(mock.Anything, "workload-id", 3, "prometheus").
			Return(database.Token{}, database.ErrTokenNotFound).Once()
		tokens.EXPECT().DeleteMinted(mock.Anything, "workload-id", "prometheus", (*int)(nil), mock.Anything).
			Return(nil).Once()
		tokens.EXPECT().Create(mock.Anything, mock.Anything).
			Return(database.Token{ID: "id"}, nil).Once()

		_, refreshed, err := newTestTokenService(t, tokens).
			RefreshWorkloadToken(t.Context(), "prometheus", "workload-id", 3, true)
		require.NoError(t, err)
		assert.True(t, refreshed)
	})
}

func TestTokenService_Revoke(t *testing.T) {
	t.Parallel()

	t.Run("revokes everything a workload holds", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().DeleteForWorkload(mock.Anything, "workload-id").Return(nil).Once()

		assert.NoError(t, newTestTokenService(t, tokens).RevokeForWorkload(t.Context(), "workload-id"))
	})

	t.Run("revokes what one instance holds", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().DeleteForInstance(mock.Anything, "workload-id", 2).Return(nil).Once()

		assert.NoError(t, newTestTokenService(t, tokens).RevokeForInstance(t.Context(), "workload-id", 2))
	})

	t.Run("retires the versions a replacement superseded", func(t *testing.T) {
		tokens := NewMockTokenRepository(t)
		tokens.EXPECT().DeleteSuperseded(mock.Anything, "workload-id", 4).Return(1, nil).Once()

		assert.NoError(t, newTestTokenService(t, tokens).RevokeSuperseded(t.Context(), "workload-id", 4))
	})
}
