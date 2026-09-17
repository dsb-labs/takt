//go:build linux

package exec_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/driver/exec"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestEnforceable(t *testing.T) {
	t.Parallel()

	// Every other test in this file enforces something successfully, so the suite
	// has to have been started inside a delegated subtree — which is what running
	// it through "make test" or scripts/delegated.sh does. Asserted rather than
	// skipped for the reason TestConfinable asserts: a host this fails on fails
	// the rest of the file too.
	assert.NoError(t, exec.Enforceable(), `no delegated cgroup subtree: run the suite through "make test" or scripts/delegated.sh`)
}

func TestDriver_ResourceLimits(t *testing.T) {
	t.Parallel()

	t.Run("runs a limited workload in a cgroup carrying its limits", func(t *testing.T) {
		d, _ := newDriver(t)

		w := workload(t, "example", 1, "hash-one", "sleep 60")
		w.Spec.Resources = &manifest.Resources{Memory: "32m", CPU: 0.5, Pids: 5}

		pid, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		// Read through the process rather than through the driver's internals, so
		// what is asserted is what the kernel is actually enforcing on the command.
		path := cgroupOf(t, pid)
		assert.Equal(t, "takt-"+idOf(t)+"-0-1", filepath.Base(path))
		assert.Equal(t, strconv.Itoa(32*1024*1024), limitOf(t, path, "memory.max"))
		assert.Equal(t, "0", limitOf(t, path, "memory.swap.max"))
		assert.Equal(t, "50000 100000", limitOf(t, path, "cpu.max"))
		assert.Equal(t, "5", limitOf(t, path, "pids.max"))

		require.NoError(t, d.Stop(t.Context(), idOf(t), "example"))

		// The cgroup goes with the workload. One left behind would accumulate per
		// stopped workload until the subtree filled with empty directories.
		_, err = os.Stat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("refuses forks past the process limit", func(t *testing.T) {
		d, _ := newDriver(t)

		// Two processes: the shell and one child. The loop asks for more, the
		// kernel refuses each further fork, and the shell reports the refusal on
		// stderr — which lands in the workload's log. The report is what is waited
		// for, because which of them a shell prints varies but every wording names
		// the fork.
		//
		// The script forks nothing until it reads the limit from its own cgroup,
		// because the limit is written as the command starts and a fast shell can
		// fork ahead of it — the driver's trampoline allowance covers that moment.
		// Builtins only until then: reading through redirection forks nothing, so
		// the check cannot be refused by the limit it waits for.
		w := workload(t, "example", 1, "hash-one",
			`read line < /proc/self/cgroup
			limit="/sys/fs/cgroup${line#0::}/pids.max"
			while read max < "$limit"; [ "$max" != "2" ]; do sleep 0.1; done
			for i in 1 2 3 4 5; do sleep 30 & done
			wait`)
		w.Spec.Resources = &manifest.Resources{Pids: 2}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitOutput(t, d, "example", "fork")

		require.NoError(t, d.Stop(t.Context(), idOf(t), "example"))
	})

	t.Run("removes the cgroup when a workload is discarded", func(t *testing.T) {
		d, _ := newDriver(t)

		w := workload(t, "example", 1, "hash-one", "sleep 60")
		w.Spec.Resources = &manifest.Resources{Pids: 5}

		pid, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		path := cgroupOf(t, pid)

		require.NoError(t, d.Discard(t.Context(), idOf(t), "example"))

		_, err = os.Stat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("a workload asking for no limits runs outside any workload cgroup", func(t *testing.T) {
		d, _ := newDriver(t)

		pid, err := d.Start(t.Context(), workload(t, "example", 1, "hash-one", "sleep 60"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		// The unlimited path has to keep working on a host with no delegation at
		// all: the command inherits whatever cgroup the server runs in, exactly
		// as before limits existed.
		assert.NotContains(t, filepath.Base(cgroupOf(t, pid)), "takt-"+idOf(t))

		require.NoError(t, d.Stop(t.Context(), idOf(t), "example"))
	})
}

// A cgroup is named for the workload's identifier, instance and version, and every
// test here shares the first two. The versions differ from those the tests beside
// this one use, so that two tests running in parallel cannot end up sharing a
// cgroup — removing one ends whatever it holds, which would be the other's process.
func TestDriver_Usage(t *testing.T) {
	t.Parallel()

	t.Run("reports what a limited workload is consuming", func(t *testing.T) {
		d, _ := newDriver(t)

		// A shell busying itself, so there is processor time to report as well as
		// memory. Read from the kernel's own counters, so what is asserted is what
		// the limits are enforced against.
		w := workload(t, "example", 7, "hash-one", "while :; do :; done")
		w.Spec.Resources = &manifest.Resources{Memory: "32m", CPU: 0.5, Pids: 5}

		pid, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		// The counters are the kernel's, written as the workload runs, so the first
		// reading can arrive before the shell has been charged for anything.
		var usage driver.Usage

		require.Eventually(t, func() bool {
			readings, err := d.Usage(t.Context(), idOf(t), "example")
			require.NoError(t, err)

			usage = readings[pid]

			return usage.CPU > 0
		}, time.Second*5, time.Millisecond*50)

		assert.Positive(t, usage.Memory)
		assert.GreaterOrEqual(t, usage.Pids, 1)
		assert.False(t, usage.At.IsZero())

		require.NoError(t, d.Stop(t.Context(), idOf(t), "example"))
	})

	// An unlimited workload runs in the server's own cgroup, so the only counters
	// there describe the server. Reporting those as the workload's would be worse
	// than reporting nothing.
	t.Run("reports nothing for a workload asking for no limits", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload(t, "example", 8, "hash-one", "sleep 60"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		usage, err := d.Usage(t.Context(), idOf(t), "example")
		require.NoError(t, err)
		assert.Empty(t, usage)

		require.NoError(t, d.Stop(t.Context(), idOf(t), "example"))
	})

	t.Run("reports nothing for a workload that has stopped", func(t *testing.T) {
		d, _ := newDriver(t)

		w := workload(t, "example", 9, "hash-one", "sleep 60")
		w.Spec.Resources = &manifest.Resources{Pids: 5}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)
		require.NoError(t, d.Stop(t.Context(), idOf(t), "example"))

		usage, err := d.Usage(t.Context(), idOf(t), "example")
		require.NoError(t, err)
		assert.Empty(t, usage)
	})
}

// cgroupOf returns the cgroup a process is in, read from the process itself.
func cgroupOf(t *testing.T, pid string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
	require.NoError(t, err)

	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", rest)
		}
	}

	t.Fatalf("process %s is not in a cgroup2 hierarchy", pid)

	return ""
}

// limitOf reads one limit from a cgroup.
func limitOf(t *testing.T, path, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(path, name))
	require.NoError(t, err)

	return strings.TrimSpace(string(data))
}
