package database_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
)

func TestTokenRepository(t *testing.T) {
	t.Parallel()

	t.Run("creates and loads a token by its hash", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		created, err := tokens.Create(ctx, database.Token{
			Hash:      "hash-1",
			Type:      "client",
			Source:    "static",
			Principal: "prometheus",
			Groups:    []string{"infra"},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, created.ID)
		assert.False(t, created.CreatedAt.IsZero())

		loaded, err := tokens.GetByHash(ctx, "hash-1")
		require.NoError(t, err)
		assert.Equal(t, created.ID, loaded.ID)
		assert.Equal(t, "client", loaded.Type)
		assert.Equal(t, "static", loaded.Source)
		assert.Equal(t, "prometheus", loaded.Principal)
		assert.Equal(t, []string{"infra"}, loaded.Groups)
		assert.True(t, loaded.ExpiresAt.IsZero())
		assert.True(t, loaded.LastUsedAt.IsZero())
	})

	t.Run("reports an unknown hash", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))

		_, err := tokens.GetByHash(t.Context(), "missing")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)
	})

	// `takt acl init` works exactly once because the schema refuses a second
	// recovery row, not because any code counts them.
	t.Run("refuses a second recovery token", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := tokens.Create(ctx, database.Token{Hash: "recovery-1", Type: "recovery", Source: "init"})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{Hash: "recovery-2", Type: "recovery", Source: "init"})
		assert.ErrorIs(t, err, database.ErrTokenAlreadyExists)
	})

	t.Run("deletes a token by identifier", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		created, err := tokens.Create(ctx, database.Token{Hash: "hash-1", Type: "client", Source: "static", Principal: "ci"})
		require.NoError(t, err)

		require.NoError(t, tokens.Delete(ctx, created.ID))

		_, err = tokens.GetByHash(ctx, "hash-1")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)
		assert.ErrorIs(t, tokens.Delete(ctx, created.ID), database.ErrTokenNotFound)
	})

	t.Run("deletes the recovery token without touching client tokens", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := tokens.Create(ctx, database.Token{Hash: "recovery-1", Type: "recovery", Source: "init"})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{Hash: "hash-1", Type: "client", Source: "static", Principal: "ci"})
		require.NoError(t, err)

		require.NoError(t, tokens.DeleteRecovery(ctx))
		// The reset file asks for a state rather than an action, so deleting
		// again is not an error.
		require.NoError(t, tokens.DeleteRecovery(ctx))

		_, err = tokens.GetByHash(ctx, "recovery-1")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)

		_, err = tokens.GetByHash(ctx, "hash-1")
		assert.NoError(t, err)
	})

	t.Run("records when a token was last used", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		created, err := tokens.Create(ctx, database.Token{Hash: "hash-1", Type: "client", Source: "static", Principal: "ci"})
		require.NoError(t, err)

		used := time.Now().UTC().Truncate(time.Second)
		require.NoError(t, tokens.Touch(ctx, created.ID, used))

		loaded, err := tokens.GetByHash(ctx, "hash-1")
		require.NoError(t, err)
		assert.Equal(t, used, loaded.LastUsedAt)
	})

	t.Run("lists tokens newest first", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := tokens.Create(ctx, database.Token{Hash: "hash-1", Type: "client", Source: "static", Principal: "one"})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{Hash: "hash-2", Type: "client", Source: "static", Principal: "two"})
		require.NoError(t, err)

		listed, err := tokens.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 2)
		assert.Equal(t, "two", listed[0].Principal)
		assert.Equal(t, "one", listed[1].Principal)
	})

	t.Run("deletes expired tokens and keeps the rest", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))
		ctx := t.Context()

		now := time.Now().UTC()

		_, err := tokens.Create(ctx, database.Token{
			Hash: "hash-1", Type: "client", Source: "session", Principal: "old",
			ExpiresAt: now.Add(-time.Hour),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "hash-2", Type: "client", Source: "session", Principal: "fresh",
			ExpiresAt: now.Add(time.Hour),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{Hash: "hash-3", Type: "client", Source: "static", Principal: "forever"})
		require.NoError(t, err)

		deleted, err := tokens.DeleteExpired(ctx, now)
		require.NoError(t, err)
		assert.EqualValues(t, 1, deleted)

		listed, err := tokens.List(ctx)
		require.NoError(t, err)
		assert.Len(t, listed, 2)
	})
}
