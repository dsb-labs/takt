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
