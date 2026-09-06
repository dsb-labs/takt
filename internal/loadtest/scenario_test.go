package loadtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/loadtest"
)

func TestParseScenario(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		File         string
		Expected     loadtest.Scenario
		ExpectsError bool
	}{
		{
			Name: "full & valid",
			File: "full.toml",
			Expected: loadtest.Scenario{
				Name:        "example",
				Description: "Everything a scenario can say.",
				Fleet: loadtest.Fleet{
					Containers:     8,
					Exec:           4,
					DynamicPorts:   0.5,
					FixedPorts:     0.25,
					HealthChecks:   0.5,
					Scheduled:      0.1,
					Failing:        0.05,
					ReadsSecret:    1,
					ReadsVariable:  1,
					MountsSecret:   0.25,
					MountsVariable: 0.25,
					MountsVolume:   0.25,
				},
				Resources: loadtest.Resources{Secrets: 3, Variables: 3, Volumes: 2, Services: 2},
				Churn: loadtest.Churn{
					Duration: 30 * time.Second,
					Workers:  4,
					Weights: loadtest.Weights{
						RotateSecret:   2,
						RotateVariable: 1,
						Reapply:        2,
						List:           2,
						Get:            1,
						Logs:           1,
						Restart:        1,
						GetService:     1,
					},
				},
			},
		},
		{
			// A scenario that applies a fleet and waits for it is about convergence
			// rather than load, and needs no churn at all.
			Name: "minimal",
			File: "minimal.toml",
			Expected: loadtest.Scenario{
				Name:  "minimal",
				Fleet: loadtest.Fleet{Containers: 1},
			},
		},
		{
			// The reason unknown keys are refused: a mistyped proportion that did
			// nothing would leave a scenario claiming coverage it does not have.
			Name:         "unknown key",
			File:         "unknown-key.toml",
			ExpectsError: true,
		},
		{
			Name:         "proportion above one",
			File:         "proportion-too-high.toml",
			ExpectsError: true,
		},
		{
			Name:         "reads a secret the scenario never creates",
			File:         "unreadable.toml",
			ExpectsError: true,
		},
		{
			Name:         "no workloads",
			File:         "empty-fleet.toml",
			ExpectsError: true,
		},
		{
			Name:         "churn with no weighted operation",
			File:         "no-weights.toml",
			ExpectsError: true,
		},
		{
			Name:         "malformed",
			File:         "malformed.toml",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.File))
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })

			actual, err := loadtest.ParseScenario(f)
			if tc.ExpectsError {
				assert.Zero(t, actual)
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.EqualValues(t, tc.Expected, actual)
		})
	}
}

// TestParseScenario_UnknownKeyNamesIt covers the part of the refusal that is worth
// having. Being told a key is unknown is only useful with the key in the message.
func TestParseScenario_UnknownKeyNamesIt(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join("testdata", "unknown-key.toml"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	_, err = loadtest.ParseScenario(f)
	require.ErrorIs(t, err, loadtest.ErrInvalidScenario)
	assert.Contains(t, err.Error(), "fleet.mounts-secrets")
}

func TestScenario_Workloads(t *testing.T) {
	t.Parallel()

	scenario := loadtest.Scenario{Fleet: loadtest.Fleet{Containers: 80, Exec: 40}}
	assert.Equal(t, 120, scenario.Workloads())
}

// TestScenario_Validate covers the rules a file cannot reach, because they are about
// combinations rather than about any one value.
func TestScenario_Validate(t *testing.T) {
	t.Parallel()

	valid := func() loadtest.Scenario {
		return loadtest.Scenario{
			Name:  "example",
			Fleet: loadtest.Fleet{Containers: 1},
		}
	}

	t.Run("accepts a fleet with no churn", func(t *testing.T) {
		assert.NoError(t, valid().Validate())
	})

	// A workload publishes one port or none, so a fleet asking for more of both than
	// there are workloads describes something that cannot be built.
	t.Run("refuses more ported workloads than workloads", func(t *testing.T) {
		scenario := valid()
		scenario.Fleet.DynamicPorts = 0.7
		scenario.Fleet.FixedPorts = 0.7

		err := scenario.Validate()
		require.ErrorIs(t, err, loadtest.ErrInvalidScenario)
		assert.Contains(t, err.Error(), "cannot exceed 1 together")
	})

	// One negative weight cancelling a positive one would leave a mix that totals
	// zero reading as a populated one.
	t.Run("refuses a negative weight", func(t *testing.T) {
		scenario := valid()
		scenario.Churn = loadtest.Churn{
			Duration: time.Second,
			Workers:  1,
			Weights:  loadtest.Weights{List: 2, Get: -2},
		}

		err := scenario.Validate()
		require.ErrorIs(t, err, loadtest.ErrInvalidScenario)
		assert.Contains(t, err.Error(), "cannot be negative")
	})

	t.Run("refuses a churn with no workers", func(t *testing.T) {
		scenario := valid()
		scenario.Churn = loadtest.Churn{
			Duration: time.Second,
			Weights:  loadtest.Weights{List: 1},
		}

		assert.ErrorIs(t, scenario.Validate(), loadtest.ErrInvalidScenario)
	})

	t.Run("names every rule it broke", func(t *testing.T) {
		scenario := loadtest.Scenario{}

		err := scenario.Validate()
		require.Error(t, err)

		// Joined rather than returned one at a time, so a scenario written from
		// scratch is corrected in one pass rather than one rule per run.
		assert.Contains(t, err.Error(), "name is required")
		assert.Contains(t, err.Error(), "at least one workload")
	})
}

// TestScenarios covers the scenarios shipped with the repository. A library nobody
// parses is a library of files that stopped working.
func TestScenarios(t *testing.T) {
	t.Parallel()

	for name, scenario := range shippedScenarios(t) {
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, scenario.Name, "a scenario carries its name into the report")
			assert.NotEmpty(t, scenario.Description, "a scenario says what it is for")
		})
	}
}

// shippedScenarios parses every scenario in the library, keyed by file name.
func shippedScenarios(t *testing.T) map[string]loadtest.Scenario {
	t.Helper()

	dir := filepath.Join("..", "..", "scenarios")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the scenario library is empty")

	scenarios := make(map[string]loadtest.Scenario, len(entries))

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}

		f, err := os.Open(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })

		scenario, err := loadtest.ParseScenario(f)
		require.NoError(t, err, "scenario %s does not parse", entry.Name())

		scenarios[entry.Name()] = scenario
	}

	return scenarios
}
