package loadtest_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/loadtest"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// TestBuild_Valid is the test that matters most here. A fleet the server refuses is a
// scenario that measures the apply path rejecting things, so every specification a
// scenario can produce has to pass the same validation an operator's manifest does.
func TestBuild_Valid(t *testing.T) {
	t.Parallel()

	scenario := loadtest.Scenario{
		Name: "example",
		Fleet: loadtest.Fleet{
			Containers:     20,
			Exec:           20,
			DynamicPorts:   0.3,
			FixedPorts:     0.2,
			HealthChecks:   0.25,
			Scheduled:      0.1,
			Failing:        0.05,
			ReadsSecret:    1,
			ReadsVariable:  1,
			MountsSecret:   0.25,
			MountsVariable: 0.25,
			MountsVolume:   0.25,
		},
		Resources: loadtest.Resources{Secrets: 4, Variables: 4, Volumes: 3},
	}

	workloads, names := loadtest.Build(scenario, "test")
	require.Len(t, workloads, 40)
	require.Len(t, names.Workloads, 40)

	for _, workload := range workloads {
		assert.NoError(t, manifest.Validate(workload.Spec), "workload %s", workload.Spec.Name)
	}
}

// TestBuild_EveryScenarioIsValid runs the same check over the library, so a scenario
// somebody adds cannot describe a fleet the server would refuse.
func TestBuild_EveryScenarioIsValid(t *testing.T) {
	t.Parallel()

	for name, scenario := range shippedScenarios(t) {
		t.Run(name, func(t *testing.T) {
			workloads, _ := loadtest.Build(scenario, "test")
			require.NotEmpty(t, workloads)

			for _, workload := range workloads {
				assert.NoError(t, manifest.Validate(workload.Spec), "workload %s", workload.Spec.Name)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	t.Parallel()

	base := func() loadtest.Scenario {
		return loadtest.Scenario{
			Name:      "example",
			Fleet:     loadtest.Fleet{Containers: 10},
			Resources: loadtest.Resources{Secrets: 2, Variables: 2, Volumes: 2},
		}
	}

	t.Run("names everything the scenario asked for", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Exec = 5

		workloads, names := loadtest.Build(scenario, "run")
		assert.Len(t, workloads, 15)
		assert.Len(t, names.Secrets, 2)
		assert.Len(t, names.Variables, 2)
		assert.Len(t, names.Volumes, 2)

		// The prefix scopes a run, so two of them on one server neither collide nor
		// tear down each other's work.
		for _, name := range names.Workloads {
			assert.Contains(t, name, "run-")
		}
	})

	t.Run("gives the runtimes the scenario asked for", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Exec = 5

		workloads, _ := loadtest.Build(scenario, "run")

		var containers, execs int
		for _, workload := range workloads {
			if workload.Spec.Container != nil {
				containers++
			}
			if workload.Spec.Exec != nil {
				execs++
			}
		}

		assert.Equal(t, 10, containers)
		assert.Equal(t, 5, execs)
	})

	// Deterministic rather than random, so two runs of one scenario apply the same
	// fleet and their numbers are comparable.
	t.Run("builds the same fleet every time", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.DynamicPorts = 0.3
		scenario.Fleet.MountsSecret = 0.5

		first, _ := loadtest.Build(scenario, "run")
		second, _ := loadtest.Build(scenario, "run")

		assert.EqualValues(t, first, second)
	})

	t.Run("gives a proportion of the fleet each feature", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Containers = 20
		scenario.Fleet.DynamicPorts = 0.5
		scenario.Fleet.MountsVolume = 0.25

		workloads, _ := loadtest.Build(scenario, "run")

		var ported, mounted int
		for _, workload := range workloads {
			if len(workload.Spec.Ports) > 0 {
				ported++
			}

			for _, volume := range workload.Spec.Volumes {
				if volume.Name != "" {
					mounted++
				}
			}
		}

		assert.Equal(t, 10, ported)
		assert.Equal(t, 5, mounted)
	})

	// A workload publishes one port or none, so the two shares are laid end to end.
	t.Run("keeps dynamic and fixed ports apart", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Containers = 20
		scenario.Fleet.DynamicPorts = 0.25
		scenario.Fleet.FixedPorts = 0.25

		workloads, _ := loadtest.Build(scenario, "run")

		var dynamic, fixed int
		for _, workload := range workloads {
			for _, p := range workload.Spec.Ports {
				if p.From == 0 {
					dynamic++
				} else {
					fixed++
				}
			}
		}

		assert.Equal(t, 5, dynamic)
		assert.Equal(t, 5, fixed)
	})

	// A workload built to exit non-zero never reaches running, so counting it as
	// unconverged would report the scenario working as the scenario failing.
	t.Run("marks the workloads built to fail", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Containers = 20
		scenario.Fleet.Failing = 0.1

		workloads, _ := loadtest.Build(scenario, "run")

		var failing int
		for _, workload := range workloads {
			if workload.Fails {
				failing++
			}
		}

		assert.Equal(t, 2, failing)
	})

	// A check reaches a port, so a workload without one cannot carry a check. The
	// manifest refuses the combination, which would make the fleet unappliable.
	t.Run("only checks a workload that publishes something", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Containers = 20
		scenario.Fleet.DynamicPorts = 0.25
		scenario.Fleet.HealthChecks = 1

		workloads, _ := loadtest.Build(scenario, "run")

		for _, workload := range workloads {
			if workload.Spec.Health != nil {
				assert.NotEmpty(t, workload.Spec.Ports, "%s is checked but publishes nothing", workload.Spec.Name)
			}
		}
	})

	// An exec workload runs on the host, so a volume mount is a bind its runtime has
	// nowhere to perform.
	t.Run("does not mount volumes into exec workloads", func(t *testing.T) {
		scenario := base()
		scenario.Fleet.Containers = 0
		scenario.Fleet.Exec = 10
		scenario.Fleet.MountsVolume = 1

		workloads, _ := loadtest.Build(scenario, "run")

		for _, workload := range workloads {
			for _, volume := range workload.Spec.Volumes {
				assert.Empty(t, volume.Name, "%s mounts a volume", workload.Spec.Name)
			}
		}
	})

	t.Run("reads only what the scenario created", func(t *testing.T) {
		scenario := base()
		scenario.Resources = loadtest.Resources{}
		scenario.Fleet.ReadsSecret = 1
		scenario.Fleet.MountsVolume = 1

		// Validate would refuse this scenario, but Build has to be safe on its own:
		// a fleet referencing a secret nobody created cannot start.
		workloads, _ := loadtest.Build(scenario, "run")

		for _, workload := range workloads {
			assert.NotContains(t, fmt.Sprint(workload.Spec.Env), "${secret:")
			assert.Empty(t, workload.Spec.Volumes)
		}
	})
}
