package database_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
)

func TestPolicyRepository(t *testing.T) {
	t.Parallel()

	t.Run("reports no policy on an empty database", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))

		_, err := policies.Get(t.Context())
		assert.ErrorIs(t, err, database.ErrNoPolicy)
	})

	t.Run("applies the first policy against an empty tag", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		require.NoError(t, policies.Apply(ctx, []byte(`{"version":"v1"}`), "tag-1", ""))

		policy, err := policies.Get(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte(`{"version":"v1"}`), policy.Document)
		assert.Equal(t, "tag-1", policy.ETag)
		assert.False(t, policy.UpdatedAt.IsZero())
	})

	t.Run("replaces the policy when the tag matches", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		require.NoError(t, policies.Apply(ctx, []byte(`{"version":"v1"}`), "tag-1", ""))
		require.NoError(t, policies.Apply(ctx, []byte(`{"version":"v1","grants":[]}`), "tag-2", "tag-1"))

		policy, err := policies.Get(ctx)
		require.NoError(t, err)
		assert.Equal(t, "tag-2", policy.ETag)
	})

	// A stale apply is refused rather than silently clobbering a concurrent
	// one, which is the whole point of the conditional write.
	t.Run("refuses an apply conditioned on a stale tag", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		require.NoError(t, policies.Apply(ctx, []byte(`{"version":"v1"}`), "tag-1", ""))

		err := policies.Apply(ctx, []byte(`{"version":"v1"}`), "tag-2", "stale")
		assert.ErrorIs(t, err, database.ErrPolicyChanged)

		policy, err := policies.Get(ctx)
		require.NoError(t, err)
		assert.Equal(t, "tag-1", policy.ETag)
	})

	t.Run("refuses a first apply that expected a policy", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))

		err := policies.Apply(t.Context(), []byte(`{"version":"v1"}`), "tag-1", "tag-0")
		assert.ErrorIs(t, err, database.ErrPolicyChanged)
	})
}
