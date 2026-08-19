package database

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestOpen_ConcurrentWrites covers the pragmas Open sets. SQLite permits one writer
// at a time, and orca has several — the API accepting applies while the reconciler
// finishes deletions — so without a busy timeout a concurrent write fails outright
// rather than waiting its turn. A load test found this the hard way.
func TestOpen_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	db, err := Open(t.Context(), Config{
		Logger: slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: level})),
		Path:   filepath.Join(t.TempDir(), "test.db"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	repo := NewWorkloadRepository(db)

	var wg sync.WaitGroup
	errs := make(chan error, 50)

	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, _, err := repo.Upsert(t.Context(), Workload{
				Name:     fmt.Sprintf("example-%d", i),
				Runtime:  "container",
				Spec:     []byte(`{}`),
				SpecHash: fmt.Sprintf("hash-%d", i),
			})
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}

	stored, err := repo.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, stored, 50)
}

func TestMigrations(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	require.NoError(t, db.PingContext(t.Context()))

	t.Run("up", func(t *testing.T) {
		require.NoError(t, migrateUp(db))
	})

	t.Run("down", func(t *testing.T) {
		m, err := newMigrator(db)
		require.NoError(t, err)

		err = m.Down()
		if err != nil && !errors.Is(err, migrate.ErrNoChange) {
			require.NoError(t, err)
		}
	})

	t.Run("up again", func(t *testing.T) {
		require.NoError(t, migrateUp(db))
	})
}
