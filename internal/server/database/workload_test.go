package database_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
)

func TestWorkloadRepository_Upsert(t *testing.T) {
	t.Parallel()

	t.Run("creates a workload on first apply", func(t *testing.T) {
		repo := newTestRepository(t)

		created, isNew, err := repo.Upsert(t.Context(), database.Workload{
			Name:     "example",
			Runtime:  "container",
			Schedule: "*/5 * * * *",
			Spec:     []byte(`{"name":"example"}`),
			SpecHash: "hash-one",
			Labels:   map[string]string{"some-key": "some-value"},
		})

		require.NoError(t, err)
		assert.True(t, isNew)
		assert.Equal(t, 1, created.Version)
		assert.Equal(t, "container", created.Runtime)
		assert.Equal(t, "*/5 * * * *", created.Schedule)
		assert.Equal(t, map[string]string{"some-key": "some-value"}, created.Labels)
		assert.False(t, created.CreatedAt.IsZero())
		assert.Equal(t, created.CreatedAt, created.UpdatedAt)
	})

	t.Run("leaves the version alone when the spec is unchanged", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		w := database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{"name":"example"}`),
			SpecHash: "hash-one",
		}

		created, _, err := repo.Upsert(ctx, w)
		require.NoError(t, err)

		unchanged, isNew, err := repo.Upsert(ctx, w)
		require.NoError(t, err)

		assert.False(t, isNew)
		assert.Equal(t, 1, unchanged.Version)
		assert.Equal(t, created.UpdatedAt, unchanged.UpdatedAt)
	})

	t.Run("increments the version when the spec changes", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		created, _, err := repo.Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{"name":"example","image":"one"}`),
			SpecHash: "hash-one",
		})
		require.NoError(t, err)

		updated, isNew, err := repo.Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{"name":"example","image":"two"}`),
			SpecHash: "hash-two",
		})
		require.NoError(t, err)

		assert.False(t, isNew)
		assert.Equal(t, 2, updated.Version)
		assert.Equal(t, "hash-two", updated.SpecHash)
		assert.Equal(t, created.CreatedAt, updated.CreatedAt)
		assert.False(t, updated.UpdatedAt.Before(created.UpdatedAt))
	})
}

func TestWorkloadRepository_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns a stored workload", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		_, _, err := repo.Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{"name":"example"}`),
			SpecHash: "hash-one",
			Labels:   map[string]string{"some-key": "some-value"},
		})
		require.NoError(t, err)

		got, err := repo.Get(ctx, "example")
		require.NoError(t, err)

		assert.Equal(t, "example", got.Name)
		assert.Equal(t, 1, got.Version)
		assert.JSONEq(t, `{"name":"example"}`, string(got.Spec))
		assert.Equal(t, map[string]string{"some-key": "some-value"}, got.Labels)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		repo := newTestRepository(t)

		_, err := repo.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrWorkloadNotFound)
	})
}

func TestWorkloadRepository_List(t *testing.T) {
	t.Parallel()

	t.Run("returns workloads ordered by name", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		for _, name := range []string{"charlie", "alpha", "bravo"} {
			_, _, err := repo.Upsert(ctx, database.Workload{
				Name:     name,
				Runtime:  "container",
				Spec:     []byte(`{}`),
				SpecHash: "hash-" + name,
			})
			require.NoError(t, err)
		}

		got, err := repo.List(ctx)
		require.NoError(t, err)
		require.Len(t, got, 3)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
		assert.Equal(t, "charlie", got[2].Name)
	})

	t.Run("returns nothing when empty", func(t *testing.T) {
		repo := newTestRepository(t)

		got, err := repo.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestWorkloadRepository_List_Query(t *testing.T) {
	t.Parallel()

	// Three workloads whose specifications differ in labels, image and port, so a
	// query can be aimed at each kind of value.
	seed := func(t *testing.T, repo *database.WorkloadRepository) {
		t.Helper()

		specs := map[string]string{
			"alpha":   `{"name":"alpha","labels":{"app":"web","env":"prod"},"ports":[{"to":80,"from":8080}],"container":{"image":"nginx:1.27"}}`,
			"bravo":   `{"name":"bravo","labels":{"app":"api","env":"prod"},"ports":[{"to":6379}],"container":{"image":"redis:7"}}`,
			"charlie": `{"name":"charlie","labels":{"app":"web","env":"dev"},"container":{"image":"nginx:1.27"}}`,
		}

		for name, spec := range specs {
			_, _, err := repo.Upsert(t.Context(), database.Workload{
				Name:     name,
				Runtime:  "container",
				Spec:     []byte(spec),
				SpecHash: "hash-" + name,
			})
			require.NoError(t, err)
		}
	}

	tt := []struct {
		Name     string
		Queries  []database.Query
		Expected []string
	}{
		{
			Name:     "no queries returns everything",
			Expected: []string{"alpha", "bravo", "charlie"},
		},
		{
			Name:     "matches a label",
			Queries:  []database.Query{{Path: "$.labels.app", Value: "web"}},
			Expected: []string{"alpha", "charlie"},
		},
		{
			Name: "multiple queries are combined with and",
			Queries: []database.Query{
				{Path: "$.labels.app", Value: "web"},
				{Path: "$.labels.env", Value: "prod"},
			},
			Expected: []string{"alpha"},
		},
		{
			Name:     "reaches a nested value outside labels",
			Queries:  []database.Query{{Path: "$.container.image", Value: "nginx:1.27"}},
			Expected: []string{"alpha", "charlie"},
		},
		{
			Name: "matches a number written as text",
			// A caller with only strings — a CLI, a URL parameter — has to be able
			// to match a number in the specification.
			Queries:  []database.Query{{Path: "$.ports[0].to", Value: "80"}},
			Expected: []string{"alpha"},
		},
		{
			Name:     "a value nothing holds matches nothing",
			Queries:  []database.Query{{Path: "$.labels.app", Value: "nope"}},
			Expected: nil,
		},
		{
			Name: "a path absent from some workloads matches only those that have it",
			// charlie publishes no ports, so the path is missing rather than
			// different: that must skip the row rather than fail the query.
			Queries:  []database.Query{{Path: "$.ports[0].to", Value: "6379"}},
			Expected: []string{"bravo"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			repo := newTestRepository(t)
			seed(t, repo)

			got, err := repo.List(t.Context(), tc.Queries...)
			require.NoError(t, err)

			names := make([]string, 0, len(got))
			for _, workload := range got {
				names = append(names, workload.Name)
			}

			if tc.Expected == nil {
				assert.Empty(t, names)
				return
			}

			assert.Equal(t, tc.Expected, names)
		})
	}

	t.Run("reports a path sqlite cannot parse", func(t *testing.T) {
		repo := newTestRepository(t)

		// A malformed path would otherwise fail the whole query as an internal
		// error, long after the caller could be told they mistyped it.
		_, err := repo.List(t.Context(), database.Query{Path: "not a path", Value: "web"})
		assert.ErrorIs(t, err, database.ErrInvalidQueryPath)
	})
}

func TestWorkloadRepository_MarkDeleting(t *testing.T) {
	t.Parallel()

	t.Run("records the deletion without removing the row", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		_, _, err := repo.Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash-one",
		})
		require.NoError(t, err)

		marked, err := repo.MarkDeleting(ctx, "example")
		require.NoError(t, err)
		assert.False(t, marked.DeletedAt.IsZero())

		// The row has to survive so that the reconciler still knows what to tear
		// down, and so the teardown stays observable.
		stored, err := repo.Get(ctx, "example")
		require.NoError(t, err)
		assert.False(t, stored.DeletedAt.IsZero())
	})

	t.Run("is idempotent", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		_, _, err := repo.Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash-one",
		})
		require.NoError(t, err)

		first, err := repo.MarkDeleting(ctx, "example")
		require.NoError(t, err)

		second, err := repo.MarkDeleting(ctx, "example")
		require.NoError(t, err)

		// A repeated delete must not restart the clock, or a caller retrying could
		// hold a workload in teardown indefinitely.
		assert.Equal(t, first.DeletedAt, second.DeletedAt)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		repo := newTestRepository(t)

		_, err := repo.MarkDeleting(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrWorkloadNotFound)
	})
}

func TestWorkloadRepository_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes a stored workload", func(t *testing.T) {
		repo := newTestRepository(t)
		ctx := t.Context()

		_, _, err := repo.Upsert(ctx, database.Workload{
			Name:     "example",
			Runtime:  "container",
			Spec:     []byte(`{}`),
			SpecHash: "hash-one",
		})
		require.NoError(t, err)

		require.NoError(t, repo.Delete(ctx, "example"))

		_, err = repo.Get(ctx, "example")
		assert.ErrorIs(t, err, database.ErrWorkloadNotFound)
	})

	t.Run("reports a missing workload", func(t *testing.T) {
		repo := newTestRepository(t)

		err := repo.Delete(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrWorkloadNotFound)
	})
}

func newTestRepository(t *testing.T) *database.WorkloadRepository {
	t.Helper()

	return database.NewWorkloadRepository(newTestDatabase(t))
}

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}
