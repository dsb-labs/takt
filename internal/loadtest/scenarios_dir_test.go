package loadtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/loadtest"
)

// TestShippedScenariosParse pins that every scenario in the repository's
// scenarios directory parses and validates, so a scenario cannot rot without a
// test saying so.
func TestShippedScenariosParse(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(filepath.Join("..", "..", "scenarios"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()

			f, err := os.Open(filepath.Join("..", "..", "scenarios", entry.Name()))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })

			_, err = loadtest.ParseScenario(f)
			assert.NoError(t, err)
		})
	}
}
