//go:build linux

package exec_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/driver/exec"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestDriver_Probe(t *testing.T) {
	t.Parallel()

	t.Run("passes when the command exits zero", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		err := d.Probe(t.Context(), w, probe("exit 0"))
		assert.NoError(t, err)
	})

	t.Run("fails with the status and the output when the command exits non-zero", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		err := d.Probe(t.Context(), w, probe("echo not ready >&2; exit 3"))
		require.ErrorIs(t, err, exec.ErrProbeFailed)
		assert.Contains(t, err.Error(), "exit status 3")
		assert.Contains(t, err.Error(), "not ready")
	})

	t.Run("keeps only the end of a long output", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		// More than the driver keeps, ending in a marker that has to survive.
		err := d.Probe(t.Context(), w, probe("yes padding | head -c 4096; echo; echo MARKER; exit 1"))
		require.ErrorIs(t, err, exec.ErrProbeFailed)
		assert.Contains(t, err.Error(), "MARKER")
		assert.Less(t, len(err.Error()), 2048)
	})

	t.Run("kills a command that outlives the deadline", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()

		started := time.Now()

		// The sleep is a child of the shell, so the kill has to reach the group
		// rather than the shell alone for this to return promptly.
		err := d.Probe(ctx, w, probe("sleep 30"))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 5*time.Second)
	})

	t.Run("runs in the instance's working directory", func(t *testing.T) {
		t.Parallel()

		d, root := newDriver(t)
		w := startRunning(t, d)

		require.NoError(t, os.WriteFile(filepath.Join(root, "workloads", idOf(t), "0", "1", "cwd", "ready"), nil, 0o600))

		assert.NoError(t, d.Probe(t.Context(), w, probe("test -f ready")))
	})

	t.Run("gives the command the resolved environment", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		p := probe(`test "$EXAMPLE" = resolved`)
		p.Env = func(context.Context) (map[string]string, error) {
			return map[string]string{"EXAMPLE": "resolved"}, nil
		}

		assert.NoError(t, d.Probe(t.Context(), w, p))
	})

	t.Run("is confined as the instance is", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		secret := filepath.Join(t.TempDir(), "secret.key")
		require.NoError(t, os.WriteFile(secret, []byte("SEALED"), 0o600))

		// Readable by whoever runs the tests, so an unconfined probe would pass.
		err := d.Probe(t.Context(), w, probe("cat "+secret))
		require.ErrorIs(t, err, exec.ErrProbeFailed)
		assert.NotContains(t, err.Error(), "SEALED", "a confined probe read a file it was never granted")
		assert.Contains(t, err.Error(), "Permission denied")
	})

	t.Run("reaches the volumes the instance was started with", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		volume := newVolume(t, "example-data")

		w := workload(t, "example", 1, "hash", "sleep 60")
		w.Volumes = []driver.Volume{{Name: "example-data", Host: volume, Target: "/var/lib/example"}}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Stop(context.Background(), w.ID, w.Name) })

		awaitState(t, d, "example", driver.StateRunning)

		// Written through the link in the working directory, which the ruleset has to
		// grant by the volume's own path for the kernel to allow.
		assert.NoError(t, d.Probe(t.Context(), w, probe("echo probed > var/lib/example/file")))

		contents, err := os.ReadFile(filepath.Join(volume, "file"))
		require.NoError(t, err)
		assert.Equal(t, "probed\n", string(contents))
	})

	t.Run("does not write to the instance's log", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		require.ErrorIs(t, d.Probe(t.Context(), w, probe("echo PROBE-OUTPUT; exit 1")), exec.ErrProbeFailed)
		assert.NotContains(t, output(t, d, "example"), "PROBE-OUTPUT")
	})

	t.Run("refuses an instance that is not running", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)

		w := workload(t, "example", 1, "hash", "exit 0")

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		assert.ErrorIs(t, d.Probe(t.Context(), w, probe("exit 0")), exec.ErrNoRunningInstance)
	})

	t.Run("refuses a workload it has no record of", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)

		assert.ErrorIs(t, d.Probe(t.Context(), workload(t, "example", 1, "hash", "exit 0"), probe("exit 0")), exec.ErrNoRunningInstance)
	})
}

func TestDriver_Probe_ResourceLimits(t *testing.T) {
	t.Parallel()

	// A workload whose process limit is below what a Go runtime needs. The probe's
	// trampoline is one, so a probe started inside the instance's own cgroup could
	// not run, and this is what the probe's own cgroup exists for.
	d, _ := newDriver(t)

	w := workload(t, "example", 1, "hash", "sleep 60")
	w.Spec.Resources = &manifest.Resources{Pids: 4, Memory: "64m"}

	pid, err := d.Start(t.Context(), w)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Stop(context.Background(), w.ID, w.Name) })

	awaitState(t, d, "example", driver.StateRunning)

	// Read through the probe itself, so what is asserted is what the kernel is
	// enforcing on the command. The limits are written as the command starts, so
	// the script waits for them with builtins before it reports what it is in.
	err = d.Probe(t.Context(), w, probe(`read line < /proc/self/cgroup
		group="/sys/fs/cgroup${line#0::}"
		while read max < "$group/pids.max"; [ "$max" != "4" ]; do sleep 0.1; done
		read memory < "$group/memory.max"
		echo "$line $memory"
		[ "$memory" = "`+strconv.Itoa(64*1024*1024)+`" ] && case "$line" in *-0-1-probe) exit 0;; esac
		exit 1`))
	assert.NoError(t, err)

	// The probe's cgroup went with it, and the instance's own is untouched.
	_, err = os.Stat(cgroupOf(t, pid) + "-probe")
	assert.True(t, os.IsNotExist(err))
	assert.Equal(t, "4", limitOf(t, cgroupOf(t, pid), "pids.max"))
	awaitState(t, d, "example", driver.StateRunning)
}

// startRunning starts a long-running instance of the test's workload and returns the
// workload it was started from, stopping it when the test ends.
func startRunning(t *testing.T, d *exec.Driver) driver.Workload {
	t.Helper()

	w := workload(t, "example", 1, "hash", "sleep 60")

	_, err := d.Start(t.Context(), w)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Stop(context.Background(), w.ID, w.Name) })

	awaitState(t, d, "example", driver.StateRunning)

	return w
}

// probe describes a command check running the given shell script with an empty
// environment.
func probe(script string) driver.Probe {
	return driver.Probe{
		Command: []string{"sh", "-c", script},
		Env:     func(context.Context) (map[string]string, error) { return nil, nil },
	}
}
