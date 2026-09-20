package state_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/state"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestOf(t *testing.T) {
	t.Parallel()

	running := driver.Instance{State: driver.StateRunning}
	terminating := driver.Instance{State: driver.StateTerminating}
	failed := driver.Instance{State: driver.StateFailed, ExitCode: 1}
	exited := driver.Instance{State: driver.StateExited}
	completed := driver.Instance{State: driver.StateCompleted}

	tt := []struct {
		Name      string
		Instances []driver.Instance
		Deleting  bool
		Suspended bool
		Expected  state.Workload
	}{
		{Name: "nothing observed is pending", Expected: state.Pending},
		{Name: "running", Instances: []driver.Instance{running}, Expected: state.Running},
		// A sibling's failure is not routine, and running would mask it.
		{Name: "running beside a failure is degraded", Instances: []driver.Instance{running, failed}, Expected: state.Degraded},
		// A restart and a rolling replacement are both routine.
		{Name: "running beside a clean exit stays running", Instances: []driver.Instance{running, exited}, Expected: state.Running},
		{Name: "running beside a departing predecessor stays running", Instances: []driver.Instance{running, terminating}, Expected: state.Running},
		// The exit is a consequence of the teardown rather than news of its own.
		{Name: "terminating outranks how the departing instance ended", Instances: []driver.Instance{terminating, failed}, Expected: state.Terminating},
		{Name: "failed outranks a clean exit", Instances: []driver.Instance{exited, failed}, Expected: state.Failed},
		// The completion is true, but it is not the fact an operator needs first.
		{Name: "failed outranks a completion", Instances: []driver.Instance{completed, failed}, Expected: state.Failed},
		{Name: "exited is stopped", Instances: []driver.Instance{exited}, Expected: state.Stopped},
		{Name: "completed", Instances: []driver.Instance{completed}, Expected: state.Completed},
		// Deletion is the only thing that will happen from here, whatever runs.
		{Name: "deleting is terminating whatever runs", Instances: []driver.Instance{running}, Deleting: true, Expected: state.Terminating},
		{Name: "suspended whatever runs", Instances: []driver.Instance{running}, Suspended: true, Expected: state.Suspended},
		{Name: "deleting outranks suspended", Deleting: true, Suspended: true, Expected: state.Terminating},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.Equal(t, tc.Expected, state.Of(tc.Instances, tc.Deleting, tc.Suspended))
		})
	}
}

func TestCompletion(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Instance driver.Instance
		Policy   manifest.RestartPolicy
		Expected driver.State
	}{
		// A clean exit the policy retires is what the workload was asked for.
		{Name: "a clean exit under never is completed", Instance: driver.Instance{State: driver.StateExited}, Policy: manifest.RestartNever, Expected: driver.StateCompleted},
		{Name: "a clean exit under on-failure is completed", Instance: driver.Instance{State: driver.StateExited}, Policy: manifest.RestartOnFailure, Expected: driver.StateCompleted},
		// The policy will run it again, so it has not completed anything.
		{Name: "a clean exit under always stays exited", Instance: driver.Instance{State: driver.StateExited}, Policy: manifest.RestartAlways, Expected: driver.StateExited},
		// How it ended and whether it runs again are separate facts.
		{Name: "a failure under never stays failed", Instance: driver.Instance{State: driver.StateFailed, ExitCode: 1}, Policy: manifest.RestartNever, Expected: driver.StateFailed},
		// The policy describes what happens when work ends, and this has not.
		{Name: "a running instance is untouched", Instance: driver.Instance{State: driver.StateRunning}, Policy: manifest.RestartNever, Expected: driver.StateRunning},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.Equal(t, tc.Expected, state.Completion(tc.Instance, &manifest.Restart{Policy: tc.Policy}))
		})
	}
}
