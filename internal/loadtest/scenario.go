// Package loadtest drives a running orca server hard enough to find out what breaks.
//
// A scenario describes the shape of a fleet rather than the content of any workload
// in it. What matters to a measurement is that eighty workloads exist, that a third
// publish an allocated port and that a quarter mount a secret — not what the eighty
// are running. The workload body is therefore the package's own, and it is trivial
// and uniform: a scenario that could name an image would eventually name a large one,
// and the run would measure the daemon's network rather than orca.
//
// Every proportion in a scenario names a subsystem it stresses, so a scenario can be
// read to see what it covers. That is the property a file full of manifests could not
// have.
package loadtest

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

var (
	// ErrInvalidScenario is returned when a scenario does not describe a run that
	// can be performed.
	ErrInvalidScenario = errors.New("invalid scenario")
)

type (
	// The Scenario type describes one load test: the fleet to apply, the resources
	// it reads, and what to do to it once it is running.
	Scenario struct {
		// The name the report carries, so a set of results says which scenario
		// produced it.
		Name string `toml:"name"`
		// What the scenario is for, in a sentence. Printed when the run starts.
		Description string `toml:"description"`
		// The fleet to apply.
		Fleet Fleet `toml:"fleet"`
		// The secrets, variables and volumes the fleet reads.
		Resources Resources `toml:"resources"`
		// What to do to the fleet once it is running.
		Churn Churn `toml:"churn"`
	}

	// The Fleet type describes the workloads a scenario applies: how many of each
	// runtime, and what proportion of them carry each feature.
	//
	// Features are proportions rather than counts so that a scenario scales by
	// changing the counts alone. Doubling the fleet keeps the mix it was written to
	// exercise.
	Fleet struct {
		// How many container workloads to apply.
		Containers int `toml:"containers"`
		// How many exec workloads to apply.
		Exec int `toml:"exec"`
		// The proportion publishing a port orca allocates, which is what puts the
		// allocator under contention.
		DynamicPorts float64 `toml:"dynamic-ports"`
		// The proportion publishing a port the specification pins. A pinned port
		// cannot be moved, so these are what collide when a range is crowded.
		FixedPorts float64 `toml:"fixed-ports"`
		// The proportion carrying a health check, which is what exercises the
		// checker and the replacement an unhealthy workload triggers.
		HealthChecks float64 `toml:"health-checks"`
		// The proportion running on a schedule rather than continuously.
		Scheduled float64 `toml:"scheduled"`
		// The proportion whose command exits non-zero, which is what exercises the
		// restart backoff and the decision to give up.
		//
		// These are left out of the check that the fleet converged, because a
		// workload that is meant to fail never will.
		Failing float64 `toml:"failing"`
		// The proportion reading a secret from their environment.
		ReadsSecret float64 `toml:"reads-secret"`
		// The proportion reading a variable from their environment.
		ReadsVariable float64 `toml:"reads-variable"`
		// The proportion mounting a secret as a file.
		MountsSecret float64 `toml:"mounts-secret"`
		// The proportion mounting a variable as a file.
		MountsVariable float64 `toml:"mounts-variable"`
		// The proportion mounting a volume.
		MountsVolume float64 `toml:"mounts-volume"`
	}

	// The Resources type describes what a scenario creates for its fleet to read.
	Resources struct {
		// How many secrets to create.
		Secrets int `toml:"secrets"`
		// How many variables to create.
		Variables int `toml:"variables"`
		// How many volumes to create.
		Volumes int `toml:"volumes"`
	}

	// The Churn type describes what is done to the fleet once it is running.
	Churn struct {
		// How long to keep going for. Zero applies the fleet, waits for it, and
		// stops — which is a scenario about convergence rather than about load.
		Duration time.Duration `toml:"duration"`
		// How many operations to run at once.
		Workers int `toml:"workers"`
		// How often each operation is chosen relative to the others.
		Weights Weights `toml:"weights"`
	}

	// The Weights type describes the mix of operations a churn performs. Each is
	// chosen in proportion to its weight, and a weight of zero never happens.
	Weights struct {
		// Setting a secret to a new value, which redeploys every workload reading
		// it. The most expensive thing an operator can do casually.
		RotateSecret int `toml:"rotate-secret"`
		// Setting a variable to a new value, which does the same.
		RotateVariable int `toml:"rotate-variable"`
		// Applying a specification that has not changed, which must be a no-op.
		Reapply int `toml:"reapply"`
		// Listing every workload.
		List int `toml:"list"`
		// Reading one workload.
		Get int `toml:"get"`
		// Reading one workload's output.
		Logs int `toml:"logs"`
		// Replacing a workload's instances.
		Restart int `toml:"restart"`
	}
)

// ParseScenario reads a scenario from r.
//
// Unknown keys are rejected rather than ignored, as they are in a manifest. A
// mistyped proportion that silently did nothing would leave a scenario reporting that
// it covers something it does not.
func ParseScenario(r io.Reader) (Scenario, error) {
	var scenario Scenario

	meta, err := toml.NewDecoder(r).Decode(&scenario)
	if err != nil {
		return Scenario{}, fmt.Errorf("failed to parse scenario: %w", err)
	}

	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}

		return Scenario{}, fmt.Errorf("%w: unknown keys: %s", ErrInvalidScenario, strings.Join(keys, ", "))
	}

	if err = scenario.Validate(); err != nil {
		return Scenario{}, err
	}

	return scenario, nil
}

// Validate reports whether the scenario describes a run that can be performed.
func (s Scenario) Validate() error {
	return errors.Join(
		s.validate(),
		s.Fleet.validate(),
		s.Churn.validate(),
		s.readable(),
	)
}

// Workloads reports how many workloads the scenario applies.
func (s Scenario) Workloads() int {
	return s.Fleet.Containers + s.Fleet.Exec
}

func (s Scenario) validate() error {
	if s.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidScenario)
	}

	switch {
	case s.Resources.Secrets < 0:
		return fmt.Errorf("%w: secrets cannot be negative", ErrInvalidScenario)
	case s.Resources.Variables < 0:
		return fmt.Errorf("%w: variables cannot be negative", ErrInvalidScenario)
	case s.Resources.Volumes < 0:
		return fmt.Errorf("%w: volumes cannot be negative", ErrInvalidScenario)
	}

	return nil
}

// readable reports whether the fleet can read what it says it reads. A workload
// referencing a secret that the scenario never creates fails to start, so a scenario
// asking for one is refused here rather than producing a fleet that cannot converge.
func (s Scenario) readable() error {
	if s.Fleet.ReadsSecret > 0 && s.Resources.Secrets == 0 {
		return fmt.Errorf("%w: reads-secret needs at least one secret", ErrInvalidScenario)
	}

	if s.Fleet.MountsSecret > 0 && s.Resources.Secrets == 0 {
		return fmt.Errorf("%w: mounts-secret needs at least one secret", ErrInvalidScenario)
	}

	if s.Fleet.ReadsVariable > 0 && s.Resources.Variables == 0 {
		return fmt.Errorf("%w: reads-variable needs at least one variable", ErrInvalidScenario)
	}

	if s.Fleet.MountsVariable > 0 && s.Resources.Variables == 0 {
		return fmt.Errorf("%w: mounts-variable needs at least one variable", ErrInvalidScenario)
	}

	if s.Fleet.MountsVolume > 0 && s.Resources.Volumes == 0 {
		return fmt.Errorf("%w: mounts-volume needs at least one volume", ErrInvalidScenario)
	}

	return nil
}

func (f Fleet) validate() error {
	if f.Containers < 0 || f.Exec < 0 {
		return fmt.Errorf("%w: workload counts cannot be negative", ErrInvalidScenario)
	}

	if f.Containers+f.Exec == 0 {
		return fmt.Errorf("%w: a fleet needs at least one workload", ErrInvalidScenario)
	}

	for name, proportion := range f.proportions() {
		if proportion < 0 || proportion > 1 {
			return fmt.Errorf("%w: %s must be between 0 and 1, got %v", ErrInvalidScenario, name, proportion)
		}
	}

	// A workload publishes one port or none, so asking for more of both than there
	// are workloads describes a fleet that cannot be built.
	if f.DynamicPorts+f.FixedPorts > 1 {
		return fmt.Errorf("%w: dynamic-ports and fixed-ports cannot exceed 1 together", ErrInvalidScenario)
	}

	return nil
}

// proportions names every proportion a fleet carries, so that validating them is one
// loop rather than one condition each.
func (f Fleet) proportions() map[string]float64 {
	return map[string]float64{
		"dynamic-ports":   f.DynamicPorts,
		"fixed-ports":     f.FixedPorts,
		"health-checks":   f.HealthChecks,
		"scheduled":       f.Scheduled,
		"failing":         f.Failing,
		"reads-secret":    f.ReadsSecret,
		"reads-variable":  f.ReadsVariable,
		"mounts-secret":   f.MountsSecret,
		"mounts-variable": f.MountsVariable,
		"mounts-volume":   f.MountsVolume,
	}
}

func (c Churn) validate() error {
	if c.Duration < 0 {
		return fmt.Errorf("%w: churn duration cannot be negative", ErrInvalidScenario)
	}

	// A scenario that only applies a fleet and waits for it is about convergence
	// rather than load, and needs neither workers nor a mix.
	if c.Duration == 0 {
		return nil
	}

	if c.Workers < 1 {
		return fmt.Errorf("%w: a churn needs at least one worker", ErrInvalidScenario)
	}

	// Negatives are refused before the total is trusted, or one cancels another out
	// and an empty mix reads as a populated one.
	for name, weight := range c.Weights.each() {
		if weight < 0 {
			return fmt.Errorf("%w: weight %s cannot be negative", ErrInvalidScenario, name)
		}
	}

	if c.Weights.total() == 0 {
		return fmt.Errorf("%w: a churn needs at least one operation with a weight", ErrInvalidScenario)
	}

	return nil
}

// each names every weighted operation, so that validating and choosing between them
// both work from one list.
func (w Weights) each() map[string]int {
	return map[string]int{
		"rotate-secret":   w.RotateSecret,
		"rotate-variable": w.RotateVariable,
		"reapply":         w.Reapply,
		"list":            w.List,
		"get":             w.Get,
		"logs":            w.Logs,
		"restart":         w.Restart,
	}
}

func (w Weights) total() int {
	var total int
	for _, weight := range w.each() {
		total += weight
	}

	return total
}
