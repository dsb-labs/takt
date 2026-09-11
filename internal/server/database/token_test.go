package database_test

import (
	"database/sql"
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

	t.Run("binds a minted token to a workload", func(t *testing.T) {
		db := newTestDatabase(t)
		tokens := database.NewTokenRepository(db)
		ctx := t.Context()

		workload := createTestWorkload(t, db, "example")

		_, err := tokens.Create(ctx, database.Token{
			Hash: "shared", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID, WorkloadVersion: new(1),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "instance-0", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID, WorkloadInstance: new(0),
		})
		require.NoError(t, err)

		// Only the token every instance shares comes back: the instance-bound one
		// is a different credential with a different life.
		loaded, err := tokens.GetForWorkload(ctx, workload.ID, 1, "prometheus")
		require.NoError(t, err)
		assert.Equal(t, "shared", loaded.Hash)
		assert.Equal(t, workload.ID, loaded.WorkloadID)
		assert.Nil(t, loaded.WorkloadInstance)
		require.NotNil(t, loaded.WorkloadVersion)
		assert.Equal(t, 1, *loaded.WorkloadVersion)

		bound, err := tokens.GetByHash(ctx, "instance-0")
		require.NoError(t, err)
		require.NotNil(t, bound.WorkloadInstance)
		assert.Equal(t, 0, *bound.WorkloadInstance)
		assert.Nil(t, bound.WorkloadVersion)
	})

	t.Run("reports a workload nothing was minted for", func(t *testing.T) {
		tokens := database.NewTokenRepository(newTestDatabase(t))

		_, err := tokens.GetForWorkload(t.Context(), "missing", 1, "prometheus")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)
	})

	t.Run("replaces only the token a mint supersedes", func(t *testing.T) {
		db := newTestDatabase(t)
		tokens := database.NewTokenRepository(db)
		ctx := t.Context()

		workload := createTestWorkload(t, db, "example")

		_, err := tokens.Create(ctx, database.Token{
			Hash: "shared", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID, WorkloadVersion: new(1),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "instance-0", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID, WorkloadInstance: new(0),
		})
		require.NoError(t, err)

		require.NoError(t, tokens.DeleteMinted(ctx, workload.ID, "prometheus", nil, new(1)))

		_, err = tokens.GetByHash(ctx, "shared")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)

		// The instance-bound token was minted for something else, so a shared
		// mint replacing its predecessor leaves it alone.
		_, err = tokens.GetByHash(ctx, "instance-0")
		assert.NoError(t, err)

		// Nothing left to replace, which is not an error: a mint replaces
		// whatever its predecessor was, including nothing.
		assert.NoError(t, tokens.DeleteMinted(ctx, workload.ID, "prometheus", nil, new(1)))
	})

	t.Run("retires the tokens a replacement superseded", func(t *testing.T) {
		db := newTestDatabase(t)
		tokens := database.NewTokenRepository(db)
		ctx := t.Context()

		workload := createTestWorkload(t, db, "example")

		_, err := tokens.Create(ctx, database.Token{
			Hash: "version-1", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID, WorkloadVersion: new(1),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "version-2", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID, WorkloadVersion: new(2),
		})
		require.NoError(t, err)

		// An instance-bound token carries no version, so a reclaim that names
		// one must not sweep it up.
		_, err = tokens.Create(ctx, database.Token{
			Hash: "instance-0", Type: "client", Source: "workload", Principal: "ci",
			WorkloadID: workload.ID, WorkloadInstance: new(0),
		})
		require.NoError(t, err)

		deleted, err := tokens.DeleteSuperseded(ctx, workload.ID, 2)
		require.NoError(t, err)
		assert.EqualValues(t, 1, deleted)

		_, err = tokens.GetByHash(ctx, "version-1")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)

		_, err = tokens.GetByHash(ctx, "version-2")
		assert.NoError(t, err)

		_, err = tokens.GetByHash(ctx, "instance-0")
		assert.NoError(t, err)
	})

	t.Run("revokes every token minted for a workload", func(t *testing.T) {
		db := newTestDatabase(t)
		tokens := database.NewTokenRepository(db)
		ctx := t.Context()

		workload := createTestWorkload(t, db, "example")

		_, err := tokens.Create(ctx, database.Token{
			Hash: "shared", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID,
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "instance-1", Type: "client", Source: "workload", Principal: "ci",
			WorkloadID: workload.ID, WorkloadInstance: new(1),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{Hash: "static", Type: "client", Source: "static", Principal: "operator"})
		require.NoError(t, err)

		require.NoError(t, tokens.DeleteForWorkload(ctx, workload.ID))

		listed, err := tokens.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "static", listed[0].Hash)

		// Revocation asks for a state, so asking again is not an error.
		assert.NoError(t, tokens.DeleteForWorkload(ctx, workload.ID))
	})

	t.Run("revokes the tokens minted for one instance", func(t *testing.T) {
		db := newTestDatabase(t)
		tokens := database.NewTokenRepository(db)
		ctx := t.Context()

		workload := createTestWorkload(t, db, "example")

		_, err := tokens.Create(ctx, database.Token{
			Hash: "shared", Type: "client", Source: "workload", Principal: "ci",
			WorkloadID: workload.ID,
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "instance-0", Type: "client", Source: "workload", Principal: "ci",
			WorkloadID: workload.ID, WorkloadInstance: new(0),
		})
		require.NoError(t, err)

		_, err = tokens.Create(ctx, database.Token{
			Hash: "instance-1", Type: "client", Source: "workload", Principal: "ci",
			WorkloadID: workload.ID, WorkloadInstance: new(1),
		})
		require.NoError(t, err)

		require.NoError(t, tokens.DeleteForInstance(ctx, workload.ID, 0))

		_, err = tokens.GetByHash(ctx, "instance-0")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)

		_, err = tokens.GetByHash(ctx, "instance-1")
		assert.NoError(t, err)

		_, err = tokens.GetByHash(ctx, "shared")
		assert.NoError(t, err)
	})

	t.Run("removes a workload's tokens with its row", func(t *testing.T) {
		// The reconciler revokes before the row goes. The cascade covers a path
		// that deletes the row without it.
		db := newTestDatabase(t)
		tokens := database.NewTokenRepository(db)
		workloads := database.NewWorkloadRepository(db)
		ctx := t.Context()

		workload := createTestWorkload(t, db, "example")

		_, err := tokens.Create(ctx, database.Token{
			Hash: "shared", Type: "client", Source: "workload", Principal: "prometheus",
			WorkloadID: workload.ID,
		})
		require.NoError(t, err)

		require.NoError(t, workloads.Delete(ctx, "example"))

		_, err = tokens.GetByHash(ctx, "shared")
		assert.ErrorIs(t, err, database.ErrTokenNotFound)
	})
}

// createTestWorkload stores a workload row for a token to be bound to, since the
// binding is a foreign key.
func createTestWorkload(t *testing.T, db *sql.DB, name string) database.Workload {
	t.Helper()

	workload, _, err := database.NewWorkloadRepository(db).Upsert(t.Context(), database.Workload{
		Name:     name,
		Runtime:  "container",
		Spec:     []byte(`{"name":"` + name + `"}`),
		SpecHash: "hash-" + name,
	})
	require.NoError(t, err)

	return workload
}
