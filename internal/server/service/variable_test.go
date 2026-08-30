package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestVariableService_Set(t *testing.T) {
	t.Parallel()

	t.Run("stores a variable that does not exist", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{}, database.ErrVariableNotFound).Once()
		variables.EXPECT().Upsert(mock.Anything, "log-level", "debug", mock.Anything).
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return(nil, nil).Once()

		stored, created, err := newTestVariableService(t, variables).Set(t.Context(), "log-level", "debug", nil)
		require.NoError(t, err)
		assert.True(t, created)
		assert.Equal(t, "log-level", stored.Name)
		assert.Equal(t, "debug", stored.Value)
	})

	t.Run("replaces the value of one that exists", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()
		variables.EXPECT().Upsert(mock.Anything, "log-level", "info", mock.Anything).
			Return(database.Variable{Name: "log-level", Value: "info"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return(nil, nil).Once()

		stored, created, err := newTestVariableService(t, variables).Set(t.Context(), "log-level", "info", nil)
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, "info", stored.Value)
	})

	t.Run("writes nothing when the value is unchanged", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return(nil, nil).Once()

		// Setting a variable to what it already holds writes nothing, so a tool that
		// sets every variable on every run does not restart the fleet each time. The
		// mock asserts no Upsert, since it was never told to expect one.
		stored, created, err := newTestVariableService(t, variables).Set(t.Context(), "log-level", "debug", nil)
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, "debug", stored.Value)
	})

	t.Run("stores a value holding nothing", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "empty").
			Return(database.Variable{}, database.ErrVariableNotFound).Once()
		variables.EXPECT().Upsert(mock.Anything, "empty", "", mock.Anything).
			Return(database.Variable{Name: "empty"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "empty").Return(nil, nil).Once()

		// An empty value is a value, and setting one is not the same as leaving the
		// variable unset.
		_, created, err := newTestVariableService(t, variables).Set(t.Context(), "empty", "", nil)
		require.NoError(t, err)
		assert.True(t, created)
	})

	t.Run("redeploys the workloads reading it", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{}, database.ErrVariableNotFound).Once()
		variables.EXPECT().Upsert(mock.Anything, "log-level", "debug", mock.Anything).
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").
			Return([]string{"one", "two"}, nil).Twice()

		var rehashed []string
		svc := service.NewVariableService(service.VariableServiceConfig{
			Logger:    newTestLogger(t),
			Variables: variables,
			Rehash: func(_ context.Context, workload string) error {
				rehashed = append(rehashed, workload)

				return nil
			},
		})

		_, _, err := svc.Set(t.Context(), "log-level", "debug", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"one", "two"}, rehashed)
	})

	t.Run("redeploys nothing when the value is unchanged", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return([]string{"example"}, nil).Once()

		var rehashed []string
		svc := service.NewVariableService(service.VariableServiceConfig{
			Logger:    newTestLogger(t),
			Variables: variables,
			Rehash: func(_ context.Context, workload string) error {
				rehashed = append(rehashed, workload)

				return nil
			},
		})

		// The single UsedBy is the one hydrate makes for the response. Nothing is
		// rehashed, because nothing about what the workload reads moved.
		_, _, err := svc.Set(t.Context(), "log-level", "debug", nil)
		require.NoError(t, err)
		assert.Empty(t, rehashed)
	})

	t.Run("refuses a name orca would not accept", func(t *testing.T) {
		_, _, err := newTestVariableService(t, NewMockVariableRepository(t)).
			Set(t.Context(), "LOG_LEVEL", "debug", nil)
		assert.ErrorIs(t, err, service.ErrInvalidVariable)
	})
}

func TestVariableService_Get(t *testing.T) {
	t.Parallel()

	t.Run("returns the value and what reads it", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return([]string{"example"}, nil).Once()

		// The value comes back, unlike a secret's. Being able to confirm what a
		// workload is configured with is the reason a variable exists.
		stored, err := newTestVariableService(t, variables).Get(t.Context(), "log-level")
		require.NoError(t, err)
		assert.Equal(t, "debug", stored.Value)
		assert.Equal(t, []string{"example"}, stored.UsedBy)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "nope").
			Return(database.Variable{}, database.ErrVariableNotFound).Once()

		_, err := newTestVariableService(t, variables).Get(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrVariableNotFound)
	})
}

func TestVariableService_List(t *testing.T) {
	t.Parallel()

	t.Run("returns every variable with its value", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().List(mock.Anything).Return([]database.Variable{
			{Name: "db-host", Value: "localhost"},
			{Name: "log-level", Value: "debug"},
		}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "db-host").Return(nil, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return([]string{"example"}, nil).Once()

		listed, err := newTestVariableService(t, variables).List(t.Context())
		require.NoError(t, err)
		require.Len(t, listed, 2)
		assert.Equal(t, "localhost", listed[0].Value)
		assert.Equal(t, "debug", listed[1].Value)
	})

	t.Run("returns nothing when none are stored", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().List(mock.Anything).Return(nil, nil).Once()

		listed, err := newTestVariableService(t, variables).List(t.Context())
		require.NoError(t, err)
		assert.Empty(t, listed)
	})
}

func TestVariableService_List_Queries(t *testing.T) {
	t.Parallel()

	t.Run("passes parsed queries to the repository", func(t *testing.T) {
		t.Parallel()

		variables := NewMockVariableRepository(t)

		variables.EXPECT().List(mock.Anything, []database.Query{{Path: "$.labels.app", Value: "web"}}).
			Return([]database.Variable{{Name: "db-host", Value: "localhost"}}, nil).Once()
		variables.EXPECT().UsedBy(mock.Anything, "db-host").Return(nil, nil).Once()

		got, err := newTestVariableService(t, variables).List(t.Context(), "$.labels.app=web")
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("rejects a query that is not path=value", func(t *testing.T) {
		t.Parallel()

		_, err := newTestVariableService(t, NewMockVariableRepository(t)).List(t.Context(), "$.labels.app")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})

	t.Run("reports a path the repository cannot parse", func(t *testing.T) {
		t.Parallel()

		variables := NewMockVariableRepository(t)

		variables.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, database.ErrInvalidQueryPath).Once()

		// A bad path is the caller's mistake, so it must not surface as a server
		// failure.
		_, err := newTestVariableService(t, variables).List(t.Context(), "nonsense=web")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})
}

func TestVariableService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes a variable nothing reads", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return(nil, nil).Once()
		variables.EXPECT().Delete(mock.Anything, "log-level").Return(nil).Once()

		require.NoError(t, newTestVariableService(t, variables).Delete(t.Context(), "log-level", false))
	})

	t.Run("refuses one a workload reads", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return([]string{"example"}, nil).Once()

		err := newTestVariableService(t, variables).Delete(t.Context(), "log-level", false)
		require.ErrorIs(t, err, service.ErrVariableInUse)

		// Naming the holder is the point: the alternative is an operator told only
		// that something is using it.
		assert.Contains(t, err.Error(), "example")
	})

	t.Run("removes one a workload reads when forced", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().UsedBy(mock.Anything, "log-level").Return([]string{"example"}, nil).Twice()
		variables.EXPECT().Delete(mock.Anything, "log-level").Return(nil).Once()

		var rehashed []string
		svc := service.NewVariableService(service.VariableServiceConfig{
			Logger:    newTestLogger(t),
			Variables: variables,
			Rehash: func(_ context.Context, workload string) error {
				rehashed = append(rehashed, workload)

				return nil
			},
		})

		require.NoError(t, svc.Delete(t.Context(), "log-level", true))

		// The workload is rehashed so that what it was started against stops
		// describing what orca holds.
		assert.Equal(t, []string{"example"}, rehashed)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().UsedBy(mock.Anything, "nope").Return(nil, nil).Once()
		variables.EXPECT().Delete(mock.Anything, "nope").Return(database.ErrVariableNotFound).Once()

		err := newTestVariableService(t, variables).Delete(t.Context(), "nope", false)
		assert.ErrorIs(t, err, service.ErrVariableNotFound)
	})
}

func TestVariableService_Value(t *testing.T) {
	t.Parallel()

	t.Run("returns what the variable holds", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "log-level").
			Return(database.Variable{Name: "log-level", Value: "debug"}, nil).Once()

		value, err := newTestVariableService(t, variables).Value(t.Context(), "log-level")
		require.NoError(t, err)
		assert.Equal(t, "debug", value)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		variables.EXPECT().Get(mock.Anything, "nope").
			Return(database.Variable{}, database.ErrVariableNotFound).Once()

		value, err := newTestVariableService(t, variables).Value(t.Context(), "nope")
		require.ErrorIs(t, err, service.ErrVariableNotFound)
		assert.Empty(t, value)
	})

	t.Run("reports a read that failed", func(t *testing.T) {
		variables := NewMockVariableRepository(t)

		failure := errors.New("database is gone")
		variables.EXPECT().Get(mock.Anything, "log-level").Return(database.Variable{}, failure).Once()

		// A read that failed is not the same as a variable nobody created, and an
		// operator told the latter would go looking for the wrong thing.
		_, err := newTestVariableService(t, variables).Value(t.Context(), "log-level")
		require.ErrorIs(t, err, failure)
		assert.NotErrorIs(t, err, service.ErrVariableNotFound)
	})
}

func newTestVariableService(t *testing.T, variables *MockVariableRepository) *service.VariableService {
	t.Helper()

	return service.NewVariableService(service.VariableServiceConfig{
		Logger:    newTestLogger(t),
		Variables: variables,
	})
}
