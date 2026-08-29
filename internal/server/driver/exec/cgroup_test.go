package exec_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/driver/exec"
	"github.com/dsb-labs/orca/pkg/manifest"
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

		w := workload("example", 1, "hash-one", "sleep 60")
		w.Spec.Resources = &manifest.Resources{Memory: "32m", CPU: 0.5, Pids: 5}

		pid, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		// Read through the process rather than through the driver's internals, so
		// what is asserted is what the kernel is actually enforcing on the command.
		path := cgroupOf(t, pid)
		assert.Equal(t, "orca-"+testID+"-1", filepath.Base(path))
		assert.Equal(t, strconv.Itoa(32*1024*1024), limitOf(t, path, "memory.max"))
		assert.Equal(t, "0", limitOf(t, path, "memory.swap.max"))
		assert.Equal(t, "50000 100000", limitOf(t, path, "cpu.max"))
		assert.Equal(t, "5", limitOf(t, path, "pids.max"))

		require.NoError(t, d.Stop(t.Context(), testID, "example"))

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
		w := workload("example", 1, "hash-one",
			`read line < /proc/self/cgroup
			limit="/sys/fs/cgroup${line#0::}/pids.max"
			while read max < "$limit"; [ "$max" != "2" ]; do sleep 0.1; done
			for i in 1 2 3 4 5; do sleep 30 & done
			wait`)
		w.Spec.Resources = &manifest.Resources{Pids: 2}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitOutput(t, d, "example", "fork")

		require.NoError(t, d.Stop(t.Context(), testID, "example"))
	})

	t.Run("removes the cgroup when a workload is discarded", func(t *testing.T) {
		d, _ := newDriver(t)

		w := workload("example", 1, "hash-one", "sleep 60")
		w.Spec.Resources = &manifest.Resources{Pids: 5}

		pid, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		path := cgroupOf(t, pid)

		require.NoError(t, d.Discard(t.Context(), testID, "example"))

		_, err = os.Stat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("a workload asking for no limits runs outside any workload cgroup", func(t *testing.T) {
		d, _ := newDriver(t)

		pid, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 60"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateRunning)

		// The unlimited path has to keep working on a host with no delegation at
		// all: the command inherits whatever cgroup the server runs in, exactly
		// as before limits existed.
		assert.NotContains(t, filepath.Base(cgroupOf(t, pid)), "orca-"+testID)

		require.NoError(t, d.Stop(t.Context(), testID, "example"))
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
