package database_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
)

func TestEncryptionKeyRepository(t *testing.T) {
	t.Parallel()

	t.Run("reports no current key on an empty database", func(t *testing.T) {
		keys := database.NewEncryptionKeyRepository(newTestDatabase(t))

		_, err := keys.Current(t.Context())
		assert.ErrorIs(t, err, database.ErrNoCurrentKey)
	})

	t.Run("adopts a key as the current one", func(t *testing.T) {
		keys := database.NewEncryptionKeyRepository(newTestDatabase(t))
		ctx := t.Context()

		require.NoError(t, keys.Adopt(ctx, "first"))

		current, err := keys.Current(ctx)
		require.NoError(t, err)
		assert.Equal(t, "first", current.ID)
		assert.True(t, current.IsCurrent)
		assert.False(t, current.CreatedAt.IsZero())
	})

	// Two current keys would mean new secrets sealed under one and the rest under
	// another, with nothing recording which. The schema is what refuses it.
	t.Run("refuses a second current key", func(t *testing.T) {
		keys := database.NewEncryptionKeyRepository(newTestDatabase(t))
		ctx := t.Context()

		require.NoError(t, keys.Adopt(ctx, "first"))
		assert.Error(t, keys.Adopt(ctx, "second"))
	})
}

// newTestKey records an encryption key and returns its identifier, so that a secret
// has something to reference.
func newTestKey(t *testing.T, db *sql.DB) string {
	t.Helper()

	const id = "test-key"

	require.NoError(t, database.NewEncryptionKeyRepository(db).Adopt(t.Context(), id))

	return id
}
