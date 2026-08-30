package database_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
)

func TestVolumeRepository_Insert(t *testing.T) {
	t.Parallel()

	t.Run("stores a volume and assigns it an identifier", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		stored, err := volumes.Insert(t.Context(), "example-data", nil)
		require.NoError(t, err)

		assert.Equal(t, "example-data", stored.Name)
		assert.NotEmpty(t, stored.ID, "the volume was given no identifier")
		assert.False(t, stored.CreatedAt.IsZero(), "the volume was given no creation time")
	})

	t.Run("refuses a name another volume holds", func(t *testing.T) {
		t.Parallel()

		// A volume holds data. Treating a repeated create as success would hand a
		// caller who meant a new name somebody else's storage.
		volumes := database.NewVolumeRepository(newTestDatabase(t))

		_, err := volumes.Insert(t.Context(), "example-data", nil)
		require.NoError(t, err)

		_, err = volumes.Insert(t.Context(), "example-data", nil)
		assert.ErrorIs(t, err, database.ErrVolumeExists)
	})
}

func TestVolumeRepository_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the stored volume", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		stored, err := volumes.Insert(t.Context(), "example-data", nil)
		require.NoError(t, err)

		got, err := volumes.Get(t.Context(), "example-data")
		require.NoError(t, err)

		assert.Equal(t, stored.ID, got.ID)
		assert.Equal(t, "example-data", got.Name)
		assert.WithinDuration(t, stored.CreatedAt, got.CreatedAt, 0)
	})

	t.Run("reports a volume that does not exist", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		_, err := volumes.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrVolumeNotFound)
	})
}

func TestVolumeRepository_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every volume by name", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		for _, name := range []string{"charlie", "alpha", "bravo"} {
			_, err := volumes.Insert(t.Context(), name, nil)
			require.NoError(t, err)
		}

		got, err := volumes.List(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 3)

		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "bravo", got[1].Name)
		assert.Equal(t, "charlie", got[2].Name)
	})

	t.Run("returns nothing when no volume exists", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		got, err := volumes.List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestVolumeRepository_List_Query(t *testing.T) {
	t.Parallel()

	// Three volumes whose labels differ, plus one with no labels at all, so a
	// query can narrow, combine and skip the unlabelled.
	seed := func(t *testing.T, volumes *database.VolumeRepository) {
		t.Helper()

		labels := map[string]map[string]string{
			"alpha":   {"app": "web", "env": "prod"},
			"bravo":   {"app": "api", "env": "prod"},
			"charlie": {"app": "web", "env": "dev"},
			"delta":   nil,
		}

		for name, volumeLabels := range labels {
			_, err := volumes.Insert(t.Context(), name, volumeLabels)
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
			Expected: []string{"alpha", "bravo", "charlie", "delta"},
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
			Name:     "a value nothing holds matches nothing",
			Queries:  []database.Query{{Path: "$.labels.app", Value: "nope"}},
			Expected: nil,
		},
		{
			Name: "a path outside labels matches nothing",
			// A volume's labels are the whole of what a query can reach. The name
			// is the handle for getting one, not something to query.
			Queries:  []database.Query{{Path: "$.name", Value: "alpha"}},
			Expected: nil,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			volumes := database.NewVolumeRepository(newTestDatabase(t))
			seed(t, volumes)

			got, err := volumes.List(t.Context(), tc.Queries...)
			require.NoError(t, err)

			names := make([]string, 0, len(got))
			for _, volume := range got {
				names = append(names, volume.Name)
			}

			if tc.Expected == nil {
				assert.Empty(t, names)
				return
			}

			assert.Equal(t, tc.Expected, names)
		})
	}

	t.Run("reports a path sqlite cannot parse", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		_, err := volumes.List(t.Context(), database.Query{Path: "not a path", Value: "web"})
		assert.ErrorIs(t, err, database.ErrInvalidQueryPath)
	})
}

func TestVolumeRepository_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes the volume", func(t *testing.T) {
		t.Parallel()

		volumes := database.NewVolumeRepository(newTestDatabase(t))

		_, err := volumes.Insert(t.Context(), "example-data", nil)
		require.NoError(t, err)

		require.NoError(t, volumes.Delete(t.Context(), "example-data"))

		_, err = volumes.Get(t.Context(), "example-data")
		assert.ErrorIs(t, err, database.ErrVolumeNotFound)
	})

	t.Run("reports a volume that does not exist", func(t *testing.T) {
		t.Parallel()

		// Deleting nothing is reported rather than silently succeeding, so that a
		// caller is told the name they typed was never there.
		volumes := database.NewVolumeRepository(newTestDatabase(t))

		assert.ErrorIs(t, volumes.Delete(t.Context(), "nope"), database.ErrVolumeNotFound)
	})
}

func TestVolumeRepository_UsedBy(t *testing.T) {
	t.Parallel()

	t.Run("names the workloads mounting the volume", func(t *testing.T) {
		t.Parallel()

		db := newTestDatabase(t)
		volumes := database.NewVolumeRepository(db)

		storeWorkload(t, db, "alpha", `{"volumes":[{"name":"shared","to":"/a"}]}`)
		storeWorkload(t, db, "bravo", `{"volumes":[{"name":"shared","to":"/b"},{"name":"other","to":"/o"}]}`)

		got, err := volumes.UsedBy(t.Context(), "shared")
		require.NoError(t, err)
		assert.Equal(t, []string{"alpha", "bravo"}, got)

		got, err = volumes.UsedBy(t.Context(), "other")
		require.NoError(t, err)
		assert.Equal(t, []string{"bravo"}, got)
	})

	t.Run("ignores a workload that mounts nothing", func(t *testing.T) {
		t.Parallel()

		// A workload's specification carries no volumes key at all when it mounts
		// nothing, and an empty array when the key survived with no entries. Neither
		// may fail the query, since most workloads look like the first.
		db := newTestDatabase(t)
		volumes := database.NewVolumeRepository(db)

		storeWorkload(t, db, "absent", `{"name":"absent"}`)
		storeWorkload(t, db, "empty", `{"volumes":[]}`)
		storeWorkload(t, db, "mounts", `{"volumes":[{"name":"shared","to":"/s"}]}`)

		got, err := volumes.UsedBy(t.Context(), "shared")
		require.NoError(t, err)
		assert.Equal(t, []string{"mounts"}, got)
	})

	t.Run("returns nothing for a volume nothing mounts", func(t *testing.T) {
		t.Parallel()

		db := newTestDatabase(t)
		volumes := database.NewVolumeRepository(db)

		storeWorkload(t, db, "alpha", `{"volumes":[{"name":"shared","to":"/a"}]}`)

		got, err := volumes.UsedBy(t.Context(), "unused")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("counts a workload that is being deleted", func(t *testing.T) {
		t.Parallel()

		// Its containers are still running until the reconciler has torn them down,
		// so the volume they mount is still in use. Removing it here would pull the
		// data out from under work that has not stopped.
		db := newTestDatabase(t)
		volumes := database.NewVolumeRepository(db)
		workloads := database.NewWorkloadRepository(db)

		storeWorkload(t, db, "going", `{"volumes":[{"name":"shared","to":"/g"}]}`)

		_, err := workloads.MarkDeleting(t.Context(), "going")
		require.NoError(t, err)

		got, err := volumes.UsedBy(t.Context(), "shared")
		require.NoError(t, err)
		assert.Equal(t, []string{"going"}, got)
	})
}

// storeWorkload writes a workload with the given specification, so that a test can
// describe the mounts it cares about without building a whole valid manifest.
func storeWorkload(t *testing.T, db *sql.DB, name, spec string) {
	t.Helper()

	workloads := database.NewWorkloadRepository(db)

	_, _, err := workloads.Upsert(t.Context(), database.Workload{
		Name:     name,
		Runtime:  "container",
		Spec:     []byte(spec),
		SpecHash: "hash-" + name,
	})
	require.NoError(t, err)
}
