package database_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
)

func TestVariableRepository_Upsert(t *testing.T) {
	t.Parallel()

	t.Run("stores a variable on first write", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))

		stored, err := variables.Upsert(t.Context(), "log-level", "debug")
		require.NoError(t, err)

		assert.Equal(t, "log-level", stored.Name)
		assert.Equal(t, "debug", stored.Value)
		assert.NotEmpty(t, stored.ID)
		assert.False(t, stored.CreatedAt.IsZero())
	})

	t.Run("replaces the value of one that exists", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		first, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		second, err := variables.Upsert(ctx, "log-level", "info")
		require.NoError(t, err)

		// The identity and creation time survive, so changing a variable does not read
		// as creating a different one.
		assert.Equal(t, first.ID, second.ID)
		assert.Equal(t, first.CreatedAt, second.CreatedAt)
		assert.Equal(t, "info", second.Value)
	})

	t.Run("stores a variable holding nothing", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "empty", "")
		require.NoError(t, err)

		// An empty value is a value. A workload reading it gets an empty environment
		// variable, which is not the same as one that is not set.
		stored, err := variables.Get(ctx, "empty")
		require.NoError(t, err)
		assert.Empty(t, stored.Value)
	})
}

func TestVariableRepository_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the stored value", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		stored, err := variables.Get(ctx, "log-level")
		require.NoError(t, err)
		assert.Equal(t, "debug", stored.Value)
	})

	t.Run("reports a variable that does not exist", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))

		_, err := variables.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrVariableNotFound)
	})
}

func TestVariableRepository_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every variable with its value", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		_, err = variables.Upsert(ctx, "db-host", "localhost")
		require.NoError(t, err)

		listed, err := variables.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 2)

		assert.Equal(t, "db-host", listed[0].Name)
		assert.Equal(t, "localhost", listed[0].Value)
		assert.Equal(t, "log-level", listed[1].Name)
		assert.Equal(t, "debug", listed[1].Value)
	})

	t.Run("returns nothing when none are stored", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))

		listed, err := variables.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, listed)
	})
}

func TestVariableRepository_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes the variable", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		require.NoError(t, variables.Delete(ctx, "log-level"))

		_, err = variables.Get(ctx, "log-level")
		assert.ErrorIs(t, err, database.ErrVariableNotFound)
	})

	t.Run("reports a variable that does not exist", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))

		err := variables.Delete(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrVariableNotFound)
	})

	t.Run("leaves the workloads referencing it linked", func(t *testing.T) {
		db := newTestDatabase(t)
		variables := database.NewVariableRepository(db)
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		linkVariableWorkload(t, db, "example", "log-level")

		require.NoError(t, variables.Delete(ctx, "log-level"))

		// The link outlives the variable so that the workload keeps reporting what it
		// is missing, and so that re-creating the variable moves its hash again.
		usedBy, err := variables.UsedBy(ctx, "log-level")
		require.NoError(t, err)
		assert.Equal(t, []string{"example"}, usedBy)
	})
}

func TestVariableRepository_Values(t *testing.T) {
	t.Parallel()

	t.Run("returns the value of each named variable", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		_, err = variables.Upsert(ctx, "db-host", "localhost")
		require.NoError(t, err)

		values, err := variables.Values(ctx, []string{"log-level", "db-host"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"log-level": "debug", "db-host": "localhost"}, values)
	})

	t.Run("omits a variable that does not exist", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "log-level", "debug")
		require.NoError(t, err)

		// The caller knows what it asked about, so what a missing variable means is
		// its decision rather than an error here.
		values, err := variables.Values(ctx, []string{"log-level", "nope"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"log-level": "debug"}, values)
	})

	t.Run("returns a variable holding nothing", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := variables.Upsert(ctx, "empty", "")
		require.NoError(t, err)

		// Present with an empty value rather than absent, since these values reach a
		// workload's hash and absence is how a missing variable is reported.
		values, err := variables.Values(ctx, []string{"empty"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"empty": ""}, values)
	})

	t.Run("asks nothing when given no names", func(t *testing.T) {
		variables := database.NewVariableRepository(newTestDatabase(t))

		values, err := variables.Values(t.Context(), nil)
		require.NoError(t, err)
		assert.Empty(t, values)
	})
}

func TestVariableRepository_UsedBy(t *testing.T) {
	t.Parallel()

	t.Run("names the workloads referencing the variable", func(t *testing.T) {
		db := newTestDatabase(t)
		variables := database.NewVariableRepository(db)

		linkVariableWorkload(t, db, "example", "log-level")
		linkVariableWorkload(t, db, "other", "log-level", "db-host")
		linkVariableWorkload(t, db, "unrelated")

		usedBy, err := variables.UsedBy(t.Context(), "log-level")
		require.NoError(t, err)
		assert.Equal(t, []string{"example", "other"}, usedBy)
	})

	t.Run("returns nothing for a variable nothing references", func(t *testing.T) {
		db := newTestDatabase(t)

		linkVariableWorkload(t, db, "example", "log-level")

		usedBy, err := database.NewVariableRepository(db).UsedBy(t.Context(), "db-host")
		require.NoError(t, err)
		assert.Empty(t, usedBy)
	})

	t.Run("forgets the references a workload dropped", func(t *testing.T) {
		db := newTestDatabase(t)
		variables := database.NewVariableRepository(db)

		linkVariableWorkload(t, db, "example", "log-level")
		linkVariableWorkload(t, db, "example")

		// The links describe the specification that was last written, so a reference
		// removed from a manifest stops counting as a use.
		usedBy, err := variables.UsedBy(t.Context(), "log-level")
		require.NoError(t, err)
		assert.Empty(t, usedBy)
	})

	t.Run("does not confuse a variable with a secret of the same name", func(t *testing.T) {
		db := newTestDatabase(t)
		ctx := t.Context()

		// The two kinds are linked through separate tables, so a name held by both
		// answers for each independently.
		_, _, err := database.NewWorkloadRepository(db).Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash-example",
			Secrets:  []string{"token"},
		})
		require.NoError(t, err)

		usedBy, err := database.NewVariableRepository(db).UsedBy(ctx, "token")
		require.NoError(t, err)
		assert.Empty(t, usedBy)

		usedBy, err = database.NewSecretRepository(db).UsedBy(ctx, "token")
		require.NoError(t, err)
		assert.Equal(t, []string{"example"}, usedBy)
	})
}

func TestWorkloadRepository_Delete_ReleasesVariableLinks(t *testing.T) {
	t.Parallel()

	db := newTestDatabase(t)
	ctx := t.Context()

	linkVariableWorkload(t, db, "example", "log-level")

	require.NoError(t, database.NewWorkloadRepository(db).Delete(ctx, "example"))

	// Deleting a workload releases its links by cascade, which also proves the
	// foreign key pragma took.
	usedBy, err := database.NewVariableRepository(db).UsedBy(ctx, "log-level")
	require.NoError(t, err)
	assert.Empty(t, usedBy)
}

func TestWorkloadRepository_Upsert_LinksBothKinds(t *testing.T) {
	t.Parallel()

	db := newTestDatabase(t)
	ctx := t.Context()

	// Both sets of links are written in the transaction that writes the workload, so
	// a workload reading one of each is recorded against both.
	_, _, err := database.NewWorkloadRepository(db).Upsert(ctx, database.Workload{
		Name:      "example",
		Runtime:   "container",
		Spec:      []byte(`{}`),
		SpecHash:  "hash-example",
		Secrets:   []string{"db-password"},
		Variables: []string{"log-level"},
	})
	require.NoError(t, err)

	usedBy, err := database.NewSecretRepository(db).UsedBy(ctx, "db-password")
	require.NoError(t, err)
	assert.Equal(t, []string{"example"}, usedBy)

	usedBy, err = database.NewVariableRepository(db).UsedBy(ctx, "log-level")
	require.NoError(t, err)
	assert.Equal(t, []string{"example"}, usedBy)
}

// linkVariableWorkload writes a workload referencing the given variables, since a
// link has to belong to a workload that exists.
//
// Re-applying the same name replaces its links, and deliberately keeps the same
// specification hash: the links describe what was last written whether or not the
// specification itself changed.
func linkVariableWorkload(t *testing.T, db *sql.DB, name string, variables ...string) {
	t.Helper()

	_, _, err := database.NewWorkloadRepository(db).Upsert(t.Context(), database.Workload{
		Name:      name,
		Runtime:   "container",
		Spec:      []byte(`{}`),
		SpecHash:  "hash-" + name,
		Variables: variables,
	})
	require.NoError(t, err)
}
