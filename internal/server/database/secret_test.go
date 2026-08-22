package database_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
)

func TestSecretRepository_Upsert(t *testing.T) {
	t.Parallel()

	t.Run("stores a secret on first write", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))

		stored, err := secrets.Upsert(t.Context(), "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		assert.Equal(t, "db-password", stored.Name)
		assert.Equal(t, "rev-one", stored.Revision)
		assert.NotEmpty(t, stored.ID)
		assert.False(t, stored.CreatedAt.IsZero())
	})

	t.Run("replaces the value and revision of one that exists", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))
		ctx := t.Context()

		first, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		second, err := secrets.Upsert(ctx, "db-password", []byte("resealed"), "rev-two")
		require.NoError(t, err)

		// The identity and creation time survive, so rotating a secret does not read
		// as creating a different one.
		assert.Equal(t, first.ID, second.ID)
		assert.Equal(t, first.CreatedAt, second.CreatedAt)
		assert.Equal(t, "rev-two", second.Revision)

		read, err := secrets.Get(ctx, "db-password")
		require.NoError(t, err)
		assert.Equal(t, []byte("resealed"), read.Value)
	})
}

func TestSecretRepository_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the stored value", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		stored, err := secrets.Get(ctx, "db-password")
		require.NoError(t, err)
		assert.Equal(t, []byte("sealed"), stored.Value)
		assert.Equal(t, "rev-one", stored.Revision)
	})

	t.Run("reports a secret that does not exist", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))

		_, err := secrets.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})
}

func TestSecretRepository_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every secret without its value", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		_, err = secrets.Upsert(ctx, "api-token", []byte("sealed-too"), "rev-two")
		require.NoError(t, err)

		listed, err := secrets.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 2)

		assert.Equal(t, "api-token", listed[0].Name)
		assert.Equal(t, "db-password", listed[1].Name)

		// A read that does not carry a value cannot leak one, and listing secrets
		// needs no decryption.
		for _, secret := range listed {
			assert.Empty(t, secret.Value)
			assert.NotEmpty(t, secret.Revision)
		}
	})

	t.Run("returns nothing when none are stored", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))

		listed, err := secrets.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, listed)
	})
}

func TestSecretRepository_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes the secret", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		require.NoError(t, secrets.Delete(ctx, "db-password"))

		_, err = secrets.Get(ctx, "db-password")
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})

	t.Run("reports a secret that does not exist", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))

		err := secrets.Delete(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})

	t.Run("leaves the workloads referencing it linked", func(t *testing.T) {
		db := newTestDatabase(t)
		secrets := database.NewSecretRepository(db)
		ctx := t.Context()

		_, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		linkWorkload(t, db, "example", "db-password")

		require.NoError(t, secrets.Delete(ctx, "db-password"))

		// The link outlives the secret so that the workload keeps reporting what it
		// is missing, and so that re-creating the secret moves its hash again.
		usedBy, err := secrets.UsedBy(ctx, "db-password")
		require.NoError(t, err)
		assert.Equal(t, []string{"example"}, usedBy)
	})
}

func TestSecretRepository_Revisions(t *testing.T) {
	t.Parallel()

	t.Run("returns the revision of each named secret", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		_, err = secrets.Upsert(ctx, "api-token", []byte("sealed-too"), "rev-two")
		require.NoError(t, err)

		revisions, err := secrets.Revisions(ctx, []string{"db-password", "api-token"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"db-password": "rev-one", "api-token": "rev-two"}, revisions)
	})

	t.Run("omits a secret that does not exist", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := secrets.Upsert(ctx, "db-password", []byte("sealed"), "rev-one")
		require.NoError(t, err)

		// The caller knows what it asked about, so what a missing secret means is
		// its decision rather than an error here.
		revisions, err := secrets.Revisions(ctx, []string{"db-password", "nope"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"db-password": "rev-one"}, revisions)
	})

	t.Run("asks nothing when given no names", func(t *testing.T) {
		secrets := database.NewSecretRepository(newTestDatabase(t))

		revisions, err := secrets.Revisions(t.Context(), nil)
		require.NoError(t, err)
		assert.Empty(t, revisions)
	})
}

func TestSecretRepository_UsedBy(t *testing.T) {
	t.Parallel()

	t.Run("names the workloads referencing the secret", func(t *testing.T) {
		db := newTestDatabase(t)
		secrets := database.NewSecretRepository(db)

		linkWorkload(t, db, "example", "db-password")
		linkWorkload(t, db, "other", "db-password", "api-token")
		linkWorkload(t, db, "unrelated")

		usedBy, err := secrets.UsedBy(t.Context(), "db-password")
		require.NoError(t, err)
		assert.Equal(t, []string{"example", "other"}, usedBy)
	})

	t.Run("returns nothing for a secret nothing references", func(t *testing.T) {
		db := newTestDatabase(t)

		linkWorkload(t, db, "example", "db-password")

		usedBy, err := database.NewSecretRepository(db).UsedBy(t.Context(), "api-token")
		require.NoError(t, err)
		assert.Empty(t, usedBy)
	})

	t.Run("forgets the references a workload dropped", func(t *testing.T) {
		db := newTestDatabase(t)
		secrets := database.NewSecretRepository(db)

		linkWorkload(t, db, "example", "db-password")
		linkWorkload(t, db, "example")

		// The links describe the specification that was last written, so a reference
		// removed from a manifest stops counting as a use.
		usedBy, err := secrets.UsedBy(t.Context(), "db-password")
		require.NoError(t, err)
		assert.Empty(t, usedBy)
	})
}

func TestWorkloadRepository_Delete_ReleasesSecretLinks(t *testing.T) {
	t.Parallel()

	db := newTestDatabase(t)
	ctx := t.Context()

	linkWorkload(t, db, "example", "db-password")

	require.NoError(t, database.NewWorkloadRepository(db).Delete(ctx, "example"))

	// Deleting a workload releases its links by cascade, which also proves the
	// foreign key pragma took.
	usedBy, err := database.NewSecretRepository(db).UsedBy(ctx, "db-password")
	require.NoError(t, err)
	assert.Empty(t, usedBy)
}

// linkWorkload writes a workload referencing the given secrets, since a link has to
// belong to a workload that exists.
//
// Re-applying the same name replaces its links, and deliberately keeps the same
// specification hash: the links describe what was last written whether or not the
// specification itself changed.
func linkWorkload(t *testing.T, db *sql.DB, name string, secrets ...string) {
	t.Helper()

	_, _, err := database.NewWorkloadRepository(db).Upsert(t.Context(), database.Workload{
		Name:     name,
		Runtime:  "container",
		Spec:     []byte(`{}`),
		SpecHash: "hash-" + name,
		Secrets:  secrets,
	})
	require.NoError(t, err)
}
