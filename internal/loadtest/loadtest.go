package loadtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// How long the fleet is given to converge before the run reports what did not.
// Generous, because a scenario applying hundreds of containers is bounded by the
// daemon rather than by orca.
const convergeTimeout = 5 * time.Minute

// The Config type contains fields used to perform a load test.
type Config struct {
	// The scenario to run.
	Scenario Scenario
	// The server to run it against.
	Client *client.Client
	// What every name the run creates begins with, so that two runs on one server
	// neither collide nor tear down each other's work.
	Prefix string
	// The server's data directory, when the run is on the server's own host. Given
	// one, the run also reports what was left on disk after teardown — which is the
	// only way to see a leak the API does not expose.
	DataDir string
	// Leaves the fleet in place instead of removing it, for a run somebody wants to
	// poke at afterwards.
	Keep bool
}

// Run performs a load test and reports what happened.
//
// An error is returned when the run could not be performed. A run that was performed
// and found problems reports them in the Report, which is what Failed asks about: the
// two are different questions, and a scenario that found a bug is a successful run.
func Run(ctx context.Context, config Config) (report Report, err error) {
	scenario := config.Scenario

	workloads, names := Build(scenario, config.Prefix)
	report = Report{
		Scenario:    scenario.Name,
		Description: scenario.Description,
		Workloads:   len(workloads),
	}

	collected := newCollector()

	if err = setup(ctx, config, collected, names); err != nil {
		return Report{}, err
	}

	// Named returns, because this fills the report in after the body has returned.
	// Assigning to a copy the return had already made was how the leak findings went
	// missing on the first run of this.
	defer func() {
		// Torn down even when the run failed, or a scenario that broke halfway
		// leaves a server full of workloads for the next run to trip over.
		if !config.Keep {
			// Deliberately not the run's context: a cancelled run still has to
			// remove what it created, and every call below would refuse to start.
			teardown(context.WithoutCancel(ctx), config, collected, names)
		}

		if err != nil {
			return
		}

		// Summarised after the teardown, so the delete timings are in the report
		// rather than collected and discarded. Tearing a fleet down is the slowest
		// thing a run does, which makes it the most worth measuring.
		report.Latencies, report.Operations = collected.report()

		if !config.Keep {
			report.Leaks = leaks(context.WithoutCancel(ctx), config, names)
		}
	}()

	started := time.Now()
	if err = apply(ctx, config, collected, workloads); err != nil {
		return Report{}, err
	}

	report.Applied = Duration(time.Since(started))

	started = time.Now()
	report.Running, report.Unconverged = converge(ctx, config, workloads)
	report.Converged = Duration(time.Since(started))

	if scenario.Churn.Duration > 0 {
		churn(ctx, config, collected, names)
	}

	return report, nil
}

func setup(ctx context.Context, config Config, collected *collector, names Names) error {
	var group errgroup.Group

	for _, name := range names.Volumes {
		group.Go(func() error {
			return collected.measure("volume.create", func() error {
				_, err := config.Client.CreateVolume(ctx, manifest.Volume{Version: "v1", Name: name})

				return err
			})
		})
	}

	for i, name := range names.Secrets {
		group.Go(func() error {
			return collected.measure("secret.set", func() error {
				_, _, err := config.Client.SetSecret(ctx, name, fmt.Appendf(nil, "value-%d", i))

				return err
			})
		})
	}

	for i, name := range names.Variables {
		group.Go(func() error {
			return collected.measure("variable.set", func() error {
				_, _, err := config.Client.SetVariable(ctx, name, fmt.Sprintf("value-%d", i))

				return err
			})
		})
	}

	if err := group.Wait(); err != nil {
		return fmt.Errorf("failed to set up the scenario's resources: %w", err)
	}

	return nil
}

// apply submits the fleet, which is what puts the apply path and the port allocator
// under contention.
//
// A refused apply fails the run rather than being counted. The fleet is what
// everything after this measures, so a scenario that cannot apply its own workloads
// has nothing to say and should report that rather than measure a smaller fleet.
//
// The workloads another one reads an address from go first, and the rest follow once
// they exist. An apply naming a workload that does not exist is refused, so a fleet
// with references applied all at once would lose every referrer that raced its
// target. The burst is preserved where it matters: each wave still goes out at once,
// and a scenario without references has one wave.
func apply(ctx context.Context, config Config, collected *collector, workloads []Workload) error {
	var referenced, rest []Workload

	for _, workload := range workloads {
		if workload.Referenced {
			referenced = append(referenced, workload)

			continue
		}

		rest = append(rest, workload)
	}

	if err := applyAll(ctx, config, collected, referenced); err != nil {
		return err
	}

	return applyAll(ctx, config, collected, rest)
}

func applyAll(ctx context.Context, config Config, collected *collector, workloads []Workload) error {
	var group errgroup.Group

	for _, workload := range workloads {
		group.Go(func() error {
			return collected.measure(operation("workload.apply", workload), func() error {
				_, _, err := config.Client.Apply(ctx, workload.Spec)

				return err
			})
		})
	}

	if err := group.Wait(); err != nil {
		return fmt.Errorf("failed to apply the fleet: %w", err)
	}

	return nil
}

// converge waits for every workload that is meant to run to be running, and reports
// the ones that never got there.
//
// Workloads built to fail are left out. One that exits non-zero on purpose never
// reaches running, so counting it would report a working scenario as a failing one.
func converge(ctx context.Context, config Config, workloads []Workload) (int, []string) {
	wanted := make(map[string]struct{}, len(workloads))
	for _, workload := range workloads {
		if !workload.Fails {
			wanted[workload.Spec.Name] = struct{}{}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()

	var running map[string]struct{}

	for {
		running = observed(ctx, config, wanted)
		if len(running) == len(wanted) {
			break
		}

		select {
		case <-ctx.Done():
			// The deadline or the caller, either of which means reporting what did
			// converge rather than waiting longer.
			return len(running), missing(wanted, running)
		case <-time.After(2 * time.Second):
		}
	}

	return len(running), nil
}

func observed(ctx context.Context, config Config, wanted map[string]struct{}) map[string]struct{} {
	running := make(map[string]struct{}, len(wanted))

	listed, err := config.Client.List(ctx)
	if err != nil {
		return running
	}

	for _, workload := range listed {
		if _, ok := wanted[workload.Name]; !ok {
			continue
		}

		if workload.State == client.WorkloadStateRunning || workload.State == client.WorkloadStateCompleted {
			running[workload.Name] = struct{}{}
		}
	}

	return running
}

func missing(wanted, running map[string]struct{}) []string {
	var absent []string
	for name := range wanted {
		if _, ok := running[name]; !ok {
			absent = append(absent, name)
		}
	}

	return absent
}

// churn runs the scenario's mix of operations against the fleet for as long as it
// asked for.
func churn(ctx context.Context, config Config, collected *collector, names Names) {
	scenario := config.Scenario

	ctx, cancel := context.WithTimeout(ctx, scenario.Churn.Duration)
	defer cancel()

	choices := weighted(scenario.Churn.Weights)

	var wg sync.WaitGroup

	for worker := range scenario.Churn.Workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// Seeded per worker so that the workers do not all perform the same
			// sequence of operations against the same workloads.
			rng := rand.New(rand.NewPCG(uint64(worker), uint64(time.Now().UnixNano())))

			for ctx.Err() == nil {
				perform(ctx, config, collected, names, choices[rng.IntN(len(choices))], rng)
			}
		}()
	}

	wg.Wait()
}

// perform runs one operation of the named kind.
func perform(ctx context.Context, config Config, collected *collector, names Names, op string, rng *rand.Rand) {
	c := config.Client

	_ = collected.measure(op, func() error {
		switch op {
		case "secret.rotate":
			_, _, err := c.SetSecret(ctx, choose(names.Secrets, rng), fmt.Appendf(nil, "rotated-%d", rng.Int64()))

			return err
		case "variable.rotate":
			_, _, err := c.SetVariable(ctx, choose(names.Variables, rng), fmt.Sprintf("rotated-%d", rng.Int64()))

			return err
		case "workload.reapply":
			workloads, _ := Build(config.Scenario, config.Prefix)
			_, _, err := c.Apply(ctx, workloads[rng.IntN(len(workloads))].Spec)

			return err
		case "workload.list":
			_, err := c.List(ctx)

			return err
		case "workload.get":
			_, err := c.Get(ctx, choose(names.Workloads, rng))

			return err
		case "workload.logs":
			return c.Logs(ctx, io.Discard, choose(names.Workloads, rng), client.WithTail(10))
		case "workload.restart":
			_, err := c.Restart(ctx, choose(names.Workloads, rng))

			return err
		default:
			return fmt.Errorf("unknown operation %q", op)
		}
	})
}

// weighted expands a mix into one entry per unit of weight, so that choosing an
// operation is one index into a slice rather than a walk over the totals.
func weighted(weights Weights) []string {
	named := map[string]int{
		"secret.rotate":    weights.RotateSecret,
		"variable.rotate":  weights.RotateVariable,
		"workload.reapply": weights.Reapply,
		"workload.list":    weights.List,
		"workload.get":     weights.Get,
		"workload.logs":    weights.Logs,
		"workload.restart": weights.Restart,
	}

	var choices []string
	for op, weight := range named {
		for range weight {
			choices = append(choices, op)
		}
	}

	// Sorted so that a worker's sequence depends on its seed rather than on the
	// order a map happened to range in.
	slices.Sort(choices)

	return choices
}

func teardown(ctx context.Context, config Config, collected *collector, names Names) {
	ctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()

	var group errgroup.Group

	// Forced, because a workload another one reads an address from is otherwise
	// refused. Teardown removes everything the run created, so the order between a
	// referrer and its target is arbitrary and waiting for one would deadlock.
	for _, name := range names.Workloads {
		group.Go(func() error {
			return collected.measure("workload.delete", func() error {
				_, err := config.Client.Delete(ctx, name, client.WithWait(), client.WithForceDeleteWorkload())

				return err
			})
		})
	}

	_ = group.Wait()

	// After the workloads, because a secret a workload still reads is refused.
	for _, name := range names.Secrets {
		_ = collected.measure("secret.delete", func() error {
			return config.Client.DeleteSecret(ctx, name)
		})
	}

	for _, name := range names.Variables {
		_ = collected.measure("variable.delete", func() error {
			return config.Client.DeleteVariable(ctx, name)
		})
	}

	for _, name := range names.Volumes {
		_ = collected.measure("volume.delete", func() error {
			return config.Client.DeleteVolume(ctx, name)
		})
	}
}

func choose(names []string, rng *rand.Rand) string {
	if len(names) == 0 {
		return ""
	}

	return names[rng.IntN(len(names))]
}

// operation names the measurement a workload's apply is recorded under, so that the
// two runtimes are reported apart. What they cost is not the same.
func operation(prefix string, workload Workload) string {
	if workload.Spec.Container != nil {
		return prefix + ".container"
	}

	return prefix + ".exec"
}

// cancelled reports whether an error is the run ending rather than the server
// refusing something.
func cancelled(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// The client wraps transport failures, and a cancelled request reaches here as
	// text rather than as a wrapped context error.
	message := err.Error()

	return strings.Contains(message, context.Canceled.Error()) ||
		strings.Contains(message, context.DeadlineExceeded.Error())
}

// leaks reports what a run left on disk, for a run given somewhere to look.
//
// Only meaningful on the server's own host. The API cannot report a directory holding
// a secret's plaintext, so a whole class of leak is invisible without this.
func leaks(ctx context.Context, config Config, names Names) []string {
	if config.DataDir == "" {
		return nil
	}

	// Deletion is asynchronous, so the disk is read once the server reports the
	// workloads gone rather than once the delete calls returned. Reading earlier
	// finds a teardown in progress and calls it a leak.
	if err := gone(ctx, config, names); err != nil {
		return []string{err.Error()}
	}

	trees := []string{
		filepath.Join("mounts", "files"),
		filepath.Join("mounts", "state"),
		filepath.Join("exec", "state"),
		filepath.Join("exec", "workloads"),
	}

	var found []string

	for _, tree := range trees {
		entries, err := os.ReadDir(filepath.Join(config.DataDir, tree))
		if err != nil {
			// Nothing was ever written here, which is not a leak.
			continue
		}

		if len(entries) > 0 {
			found = append(found, fmt.Sprintf("%s holds %d entries after teardown", tree, len(entries)))
		}
	}

	return found
}

// gone waits for the server to report that everything the run created has been torn
// down.
func gone(ctx context.Context, config Config, names Names) error {
	wanted := make(map[string]struct{}, len(names.Workloads))
	for _, name := range names.Workloads {
		wanted[name] = struct{}{}
	}

	ctx, cancel := context.WithTimeout(ctx, convergeTimeout)
	defer cancel()

	for {
		listed, err := config.Client.List(ctx)
		if err == nil {
			remaining := 0
			for _, workload := range listed {
				if _, ok := wanted[workload.Name]; ok {
					remaining++
				}
			}

			if remaining == 0 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("the server still reports workloads this run created")
		case <-time.After(time.Second):
		}
	}
}
