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

	t.Run("applies the first policy against version zero", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		applied, err := policies.Apply(ctx, []byte(`{"version":"v1"}`), 0)
		require.NoError(t, err)
		assert.Equal(t, 1, applied.Version)

		policy, err := policies.Get(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte(`{"version":"v1"}`), policy.Document)
		assert.Equal(t, 1, policy.Version)
		assert.False(t, policy.UpdatedAt.IsZero())
	})

	t.Run("replaces the policy when the version matches", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := policies.Apply(ctx, []byte(`{"version":"v1"}`), 0)
		require.NoError(t, err)

		applied, err := policies.Apply(ctx, []byte(`{"version":"v1","grants":[]}`), 1)
		require.NoError(t, err)
		assert.Equal(t, 2, applied.Version)

		policy, err := policies.Get(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, policy.Version)
		assert.Equal(t, []byte(`{"version":"v1","grants":[]}`), policy.Document)
	})

	// A pipeline applying the same file on every run keeps the tag it holds,
	// which is what lets it keep applying conditionally.
	t.Run("leaves the version alone when the document is unchanged", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		first, err := policies.Apply(ctx, []byte(`{"version":"v1"}`), 0)
		require.NoError(t, err)

		second, err := policies.Apply(ctx, []byte(`{"version":"v1"}`), 1)
		require.NoError(t, err)

		assert.Equal(t, first.Version, second.Version)
		assert.WithinDuration(t, first.UpdatedAt, second.UpdatedAt, 0)
	})

	// A stale apply is refused rather than silently clobbering a concurrent
	// one, which is the whole point of the conditional write.
	t.Run("refuses an apply conditioned on a stale version", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))
		ctx := t.Context()

		_, err := policies.Apply(ctx, []byte(`{"version":"v1"}`), 0)
		require.NoError(t, err)

		_, err = policies.Apply(ctx, []byte(`{"version":"v1","grants":[]}`), 0)
		assert.ErrorIs(t, err, database.ErrPolicyChanged)

		_, err = policies.Apply(ctx, []byte(`{"version":"v1","grants":[]}`), 2)
		assert.ErrorIs(t, err, database.ErrPolicyChanged)

		policy, err := policies.Get(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, policy.Version)
		assert.Equal(t, []byte(`{"version":"v1"}`), policy.Document)
	})

	t.Run("refuses a first apply that expected a policy", func(t *testing.T) {
		policies := database.NewPolicyRepository(newTestDatabase(t))

		_, err := policies.Apply(t.Context(), []byte(`{"version":"v1"}`), 1)
		assert.ErrorIs(t, err, database.ErrPolicyChanged)
	})
}
