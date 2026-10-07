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

	t.Run("replaces the current key when nothing is sealed under it", func(t *testing.T) {
		keys := database.NewEncryptionKeyRepository(newTestDatabase(t))
		ctx := t.Context()

		require.NoError(t, keys.Adopt(ctx, "first"))
		require.NoError(t, keys.Replace(ctx, "second"))

		current, err := keys.Current(ctx)
		require.NoError(t, err)
		assert.Equal(t, "second", current.ID)

		// The replaced key stays recorded, the way a rekey keeps the key it retired.
		recorded, err := keys.List(ctx)
		require.NoError(t, err)
		assert.Len(t, recorded, 2)
	})

	// A secret is sealed under the current key. Replacing the key while one exists
	// would leave the value unopenable with the database saying otherwise.
	t.Run("refuses to replace a key secrets are sealed under", func(t *testing.T) {
		db := newTestDatabase(t)
		keys := database.NewEncryptionKeyRepository(db)
		ctx := t.Context()

		keyID := newTestKey(t, db)
		_, err := database.NewSecretRepository(db).Upsert(ctx, database.Secret{Name: "db-password", Value: []byte("sealed"), Revision: "rev-one", KeyID: keyID}, 0)
		require.NoError(t, err)

		err = keys.Replace(ctx, "second")
		assert.ErrorIs(t, err, database.ErrSecretsSealed)

		current, err := keys.Current(ctx)
		require.NoError(t, err)
		assert.Equal(t, keyID, current.ID)
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
