//go:build linux

package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsb-labs/takt/internal/server/driver"
)

var (
	// ErrProbeFailed is returned when a probe ran and exited with a status other
	// than zero. The error carries the status and the tail of what the command
	// wrote.
	ErrProbeFailed = errors.New("probe failed")
)

// The suffix on the cgroup a probe runs in, beside the workload's own.
const probeCgroupSuffix = "-probe"

// Probe runs a command health check for one instance of a workload, returning nil
// when the command exits zero.
//
// The command runs as the instance does. It starts in the instance's working
// directory, is confined to the same ruleset — the directory, the volumes the
// instance was started with, the host's own files — through the same trampoline the
// instance went through, and is given the environment the workload asked for,
// resolved again for this run. The promise the runtime makes is that nothing a
// manifest names runs unconfined, and a check is something a manifest names.
//
// Where the instance has a cgroup, the probe gets one of its own beside it under the
// same limits, rather than joining the instance's. The limits already written there
// are the command's, and the trampoline that confines the probe is a Go runtime that
// a small process limit would refuse to start. A cgroup of its own is also what makes
// the deadline certain: killing it reaches a probe that left its process group, and
// it is removed once the probe has ended.
//
// Output is captured to a bounded buffer for the error and never reaches the
// instance's log, which is the workload's own.
func (d *Driver) Probe(ctx context.Context, w driver.Workload, probe driver.Probe) error {
	output := new(driver.Tail)

	cmd, limits, err := d.launch(ctx, w, driver.Exec{
		Command: probe.Command,
		Stdout:  output,
		Stderr:  output,
		Env:     probe.Env,
	}, probeCgroupSuffix)
	if err != nil {
		return err
	}
	defer limits.discard()

	status, err := d.wait(ctx, cmd)
	switch {
	case err != nil:
		return err
	case status != 0:
		return fmt.Errorf("%w: exit status %d%s", ErrProbeFailed, status, output.Suffix())
	default:
		return nil
	}
}
