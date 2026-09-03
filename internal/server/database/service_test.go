package database_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
)

// testService returns a service row for storing in tests, targeting workloads
// labelled app=web on 8080/tcp.
func testService(name string) database.Service {
	return database.Service{
		Name:           name,
		Labels:         map[string]string{"team": "platform"},
		TargetLabels:   map[string]string{"app": "web"},
		TargetPort:     8080,
		TargetProtocol: "tcp",
	}
}

func TestServiceRepository_Upsert(t *testing.T) {
	t.Parallel()

	t.Run("stores a service and assigns it an identifier", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		stored, created, err := services.Upsert(t.Context(), testService("example"))
		require.NoError(t, err)

		assert.True(t, created, "the first write was not reported as a create")
		assert.Equal(t, "example", stored.Name)
		assert.NotEmpty(t, stored.ID, "the service was given no identifier")
		assert.False(t, stored.CreatedAt.IsZero(), "the service was given no creation time")
		assert.False(t, stored.UpdatedAt.IsZero(), "the service was given no update time")
	})

	t.Run("replaces what a stored service says", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		first, created, err := services.Upsert(t.Context(), testService("example"))
		require.NoError(t, err)
		require.True(t, created)

		replacement := testService("example")
		replacement.TargetLabels = map[string]string{"app": "api"}
		replacement.TargetPort = 9090
		replacement.TargetProtocol = "udp"

		second, created, err := services.Upsert(t.Context(), replacement)
		require.NoError(t, err)

		assert.False(t, created, "the second write was reported as a create")
		assert.Equal(t, first.ID, second.ID, "the replacement was given a new identifier")

		got, err := services.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, map[string]string{"app": "api"}, got.TargetLabels)
		assert.Equal(t, 9090, got.TargetPort)
		assert.Equal(t, "udp", got.TargetProtocol)
		assert.WithinDuration(t, first.CreatedAt, got.CreatedAt, 0)
	})
}

func TestServiceRepository_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the stored service", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		stored, _, err := services.Upsert(t.Context(), testService("example"))
		require.NoError(t, err)

		got, err := services.Get(t.Context(), "example")
		require.NoError(t, err)

		assert.Equal(t, stored.ID, got.ID)
		assert.Equal(t, "example", got.Name)
		assert.Equal(t, map[string]string{"team": "platform"}, got.Labels)
		assert.Equal(t, map[string]string{"app": "web"}, got.TargetLabels)
		assert.Equal(t, 8080, got.TargetPort)
		assert.Equal(t, "tcp", got.TargetProtocol)
		assert.WithinDuration(t, stored.CreatedAt, got.CreatedAt, 0)
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		_, err := services.Get(t.Context(), "nope")
		assert.ErrorIs(t, err, database.ErrServiceNotFound)
	})
}

func TestServiceRepository_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every service by name", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		for _, name := range []string{"beta", "alpha"} {
			_, _, err := services.Upsert(t.Context(), testService(name))
			require.NoError(t, err)
		}

		got, err := services.List(t.Context())
		require.NoError(t, err)

		require.Len(t, got, 2)
		assert.Equal(t, "alpha", got[0].Name)
		assert.Equal(t, "beta", got[1].Name)
	})

	t.Run("filters by the service's labels", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		labelled := testService("labelled")
		labelled.Labels = map[string]string{"app.kubernetes.io/name": "web"}

		_, _, err := services.Upsert(t.Context(), labelled)
		require.NoError(t, err)

		_, _, err = services.Upsert(t.Context(), testService("other"))
		require.NoError(t, err)

		// The key is quoted in the path because label keys may contain dots,
		// which would otherwise read as path separators.
		got, err := services.List(t.Context(), database.Query{
			Path:  `$.labels."app.kubernetes.io/name"`,
			Value: "web",
		})
		require.NoError(t, err)

		require.Len(t, got, 1)
		assert.Equal(t, "labelled", got[0].Name)
	})

	t.Run("reports a path sqlite cannot parse", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		_, err := services.List(t.Context(), database.Query{Path: "not a path", Value: "x"})
		assert.ErrorIs(t, err, database.ErrInvalidQueryPath)
	})
}

func TestServiceRepository_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes the stored service", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		_, _, err := services.Upsert(t.Context(), testService("example"))
		require.NoError(t, err)

		require.NoError(t, services.Delete(t.Context(), "example"))

		_, err = services.Get(t.Context(), "example")
		assert.ErrorIs(t, err, database.ErrServiceNotFound)
	})

	t.Run("reports a service that does not exist", func(t *testing.T) {
		t.Parallel()

		services := database.NewServiceRepository(newTestDatabase(t))

		assert.ErrorIs(t, services.Delete(t.Context(), "nope"), database.ErrServiceNotFound)
	})
}
