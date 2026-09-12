//go:build linux

package exec_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/driver/exec"
	"github.com/dsb-labs/takt/internal/server/reconciler"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The driver is consumed through two interfaces, which are deliberately narrower than
// the type. Asserting both here means a change to either is a compile error rather
// than a wiring failure at startup.
var (
	_ reconciler.Driver = (*exec.Driver)(nil)
	_ service.Driver    = (*exec.Driver)(nil)
)

// TestMain lets this test binary act as a confinement trampoline.
//
// A confined workload is started by takt executing itself, so the tests need the same
// of the binary they run in: started as a trampoline it has to confine itself and
// become the command, rather than run the suite a second time inside the workload.
//
// The tests that enforce resource limits also assume a delegated cgroup subtree,
// which is a property of how the suite was started rather than of the binary. Run
// the suite through "make test" or scripts/delegated.sh, which grant one.
func TestMain(m *testing.M) {
	exec.Confine()

	os.Exit(m.Run())
}

func TestDriver_Start(t *testing.T) {
	t.Parallel()

	t.Run("runs a command and reports it exited", func(t *testing.T) {
		d, root := newDriver(t)

		id, err := d.Start(t.Context(), workload("example", 1, "hash-one", "exit 0"))
		require.NoError(t, err)
		assert.NotEmpty(t, id)

		// The process is the driver's child, so waiting for it is what collects the
		// exit code. Observing before then reports it running, which is true.
		awaitState(t, d, "example", driver.StateExited)

		// Its own directory, and only the owner's: a workload's environment reaches
		// its command line and its output.
		info, err := os.Stat(filepath.Join(root, "workloads", testID, "0", "1"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	})

	t.Run("reports a command that failed", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "exit 7"))
		require.NoError(t, err)

		// Waiting on the state alone is not enough: a process that has died but whose
		// exit has not been recorded yet also reads as failed, because an exit nothing
		// observed is treated as one. The code is what says the exit was collected.
		var instances []driver.Instance

		require.Eventually(t, func() bool {
			var err error

			instances, err = d.Observe(t.Context())

			return err == nil && len(instances) == 1 && instances[0].ExitCode != 0
		}, 10*time.Second, 50*time.Millisecond)

		assert.Equal(t, driver.StateFailed, instances[0].State)
		assert.Equal(t, 7, instances[0].ExitCode)
	})

	t.Run("refuses a workload that is not a command", func(t *testing.T) {
		d, _ := newDriver(t)

		w := workload("example", 1, "hash-one", "exit 0")
		w.Spec.Exec = nil

		_, err := d.Start(t.Context(), w)
		assert.ErrorIs(t, err, exec.ErrNotExecWorkload)
	})

	t.Run("passes only the environment the workload names", func(t *testing.T) {
		d, _ := newDriver(t)

		// HOME is set for whoever runs the tests and is not named by the workload.
		// Inheriting the server's environment would hand every workload whatever it
		// holds, which may include credentials.
		require.NotEmpty(t, os.Getenv("HOME"), "this test needs an inherited variable to check against")

		w := workload("example", 1, "hash-one", `echo "[$HOME][$EXAMPLE][$PATH]"`)
		w.Env = map[string]string{"EXAMPLE": "EXAMPLE"}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 10}))

		// HOME is absent, EXAMPLE is what the workload asked for, and PATH is supplied
		// so that a command named by anything but an absolute path can be found.
		assert.Contains(t, out.String(), "[][EXAMPLE][")
		assert.NotContains(t, out.String(), os.Getenv("HOME"))
	})
}

// TestDriver_ObserveWorkload covers reading one workload rather than the whole tree.
// The state directory is keyed by identifier, so this reads one directory where
// Observe reads every one — which is what makes a single-workload read cost the same
// whether the host runs one workload or two hundred.
func TestDriver_ObserveWorkload(t *testing.T) {
	t.Parallel()

	t.Run("reports only the workload asked for", func(t *testing.T) {
		d, root := newDriver(t)

		// Identifiers the driver will accept as directory names, which is what it
		// keys its state tree on.
		const (
			firstID  = "cvhs0dq0kqj4c9r8m1a1"
			secondID = "cvhs0dq0kqj4c9r8m1a2"
		)

		writeInstanceFor(t, root, firstID, "first", 1, map[string]any{
			"pid": os.Getpid(), "startTicks": startTicks(t, os.Getpid()),
			"specHash": "hash-one", "version": 1,
			"startedAt": time.Now().Format(time.RFC3339Nano),
		})
		writeInstanceFor(t, root, secondID, "second", 1, map[string]any{
			"pid": os.Getpid(), "startTicks": startTicks(t, os.Getpid()),
			"specHash": "hash-two", "version": 1,
			"startedAt": time.Now().Format(time.RFC3339Nano),
		})

		// Both are there, so a filter that did nothing would be indistinguishable
		// from one that worked.
		all, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, all, 2)

		instances, err := d.ObserveWorkload(t.Context(), secondID, "second")
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, "second", instances[0].Workload)
		assert.Equal(t, "hash-two", instances[0].SpecHash)
	})

	// A workload nothing has been started for is not a failure. It reads as a
	// workload with no instances, which is what it is.
	t.Run("reports nothing for a workload with no records", func(t *testing.T) {
		d, _ := newDriver(t)

		instances, err := d.ObserveWorkload(t.Context(), "cvhs0dq0kqj4c9r8m1a3", "absent")
		require.NoError(t, err)
		assert.Empty(t, instances)
	})
}

func TestDriver_Observe(t *testing.T) {
	t.Parallel()

	t.Run("reports a long-running process as running", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, driver.StateRunning, instances[0].State)
		assert.Equal(t, "hash-one", instances[0].SpecHash)
		assert.Equal(t, 1, instances[0].Version)
	})

	t.Run("reports nothing for a driver that has run nothing", func(t *testing.T) {
		d, _ := newDriver(t)

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		assert.Empty(t, instances)
	})

	t.Run("adopts a process this server did not start", func(t *testing.T) {
		d, root := newDriver(t)

		// A process with no supervisor, standing in for one that outlived the server
		// that started it. Adoption is what makes restarting takt different from
		// restarting the workloads it runs.
		pid := orphan(t)

		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        pid,
			"startTicks": startTicks(t, pid),
			"specHash":   "hash-one",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, driver.StateRunning, instances[0].State)
	})

	t.Run("refuses to adopt a pid that has been reused", func(t *testing.T) {
		d, root := newDriver(t)

		// The pid is alive, but it is not the process the record describes: the start
		// time says so. A pidfile alone cannot tell these apart, and adopting the
		// wrong process would report a workload running that is not.
		pid := orphan(t)

		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        pid,
			"startTicks": startTicks(t, pid) + 1000,
			"specHash":   "hash-one",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.NotEqual(t, driver.StateRunning, instances[0].State)
	})

	t.Run("reports a process that ended unobserved as failed", func(t *testing.T) {
		d, root := newDriver(t)

		// A record for a process that is gone, with no exit code. Only the parent can
		// collect one, so this is a workload that ended while the server was down.
		// Reported as failed: a job that may not have finished is better run again
		// than assumed complete.
		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        freePID(t),
			"startTicks": 12345,
			"specHash":   "hash-one",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, driver.StateFailed, instances[0].State)
	})

	t.Run("serves an unchanged record without re-reading it", func(t *testing.T) {
		d, root := newDriver(t)

		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        freePID(t),
			"startTicks": 12345,
			"specHash":   "hash-one",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		require.Equal(t, "hash-one", instances[0].SpecHash)

		// Replace the file's bytes with garbage of the same length and put its
		// modification time back, so its identity is unchanged while its content
		// is unreadable. A second observation reporting the record anyway proves
		// it was answered from memory rather than from the file.
		path := filepath.Join(root, "state", testID, "0", "1", "state.json")

		info, err := os.Stat(path)
		require.NoError(t, err)

		garbage := bytes.Repeat([]byte("x"), int(info.Size()))
		require.NoError(t, os.WriteFile(path, garbage, 0o600))
		require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))

		instances, err = d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, "hash-one", instances[0].SpecHash)
	})

	t.Run("notices a record that was rewritten", func(t *testing.T) {
		d, root := newDriver(t)

		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        freePID(t),
			"startTicks": 12345,
			"specHash":   "hash-one",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		require.Equal(t, "hash-one", instances[0].SpecHash)

		// A different length as well as a different value, so the rewrite is
		// noticed even on a filesystem whose timestamps are too coarse to tell
		// two quick writes apart.
		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        freePID(t),
			"startTicks": 12345,
			"specHash":   "hash-two-rewritten",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		instances, err = d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, "hash-two-rewritten", instances[0].SpecHash)
	})

	t.Run("skips a directory with no readable record", func(t *testing.T) {
		d, root := newDriver(t)

		// A half-created directory, which describes nothing that can be converged.
		require.NoError(t, os.MkdirAll(filepath.Join(root, "state", testID, "1"), 0o700))

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		assert.Empty(t, instances)
	})
}

func TestDriver_Stop(t *testing.T) {
	t.Parallel()

	t.Run("stops a running process and keeps its output", func(t *testing.T) {
		d, root := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo working; sleep 300"))
		require.NoError(t, err)

		// Waited for before the stop, or the test races the shell writing it and proves
		// nothing about what retention keeps.
		awaitOutput(t, d, "example", "working")

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		// The output survives the process, which is what makes a failed attempt
		// readable after the reconciler has replaced it.
		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 10, Previous: true}))
		assert.Contains(t, out.String(), "working")

		// The directory the process ran in does not. It holds the symlinks to the
		// workload's volumes and whatever the command wrote beside them, none of which
		// has a reader once the process has ended.
		_, err = os.Stat(filepath.Join(root, "workloads", testID, "0", "1", "cwd"))
		assert.True(t, os.IsNotExist(err), "the working directory outlived the process")
	})

	t.Run("reports the instance it kept as retained", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
		require.NoError(t, err)

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		// Reported rather than hidden, so the orphan sweep still finds it, and flagged
		// so nothing deciding what to run mistakes it for work in progress.
		assert.True(t, instances[0].Retained)
	})

	t.Run("keeps one version's output however many there have been", func(t *testing.T) {
		d, root := newDriver(t)

		// Three versions in turn, which is what a workload replaced twice leaves
		// behind. Keeping all of them would be a disk leak on a workload that crashes
		// in a loop.
		for version := 1; version <= 3; version++ {
			_, err := d.Start(t.Context(), workload("example", version, "hash-one", "echo version; sleep 300"))
			require.NoError(t, err)

			require.NoError(t, d.Stop(t.Context(), "", "example"))
		}

		for _, version := range []string{"1", "2"} {
			_, err := os.Stat(filepath.Join(root, "workloads", testID, "0", version))
			assert.Truef(t, os.IsNotExist(err), "version %s outlived the version that replaced it", version)
		}

		// Set aside as the previous attempt's rather than left where it was, so that a
		// restart at this version opens a file of its own instead of appending to it.
		_, err := os.Stat(filepath.Join(root, "workloads", testID, "0", "3", "previous.log"))
		assert.NoError(t, err, "the most recent version's output was not kept")
	})

	t.Run("stops whatever the command started", func(t *testing.T) {
		d, root := newDriver(t)

		// The command's own child, which a signal to the process alone would leave
		// running. The group is what makes stopping a workload reach all of it.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300 & echo $! > child.pid; wait"))
		require.NoError(t, err)

		child := awaitChildPID(t, root, "example", 1)

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		assert.Eventually(t, func() bool {
			return syscall.Kill(child, 0) != nil
		}, 10*time.Second, 50*time.Millisecond, "a process the command started outlived the workload")
	})

	t.Run("does nothing for a workload it has never run", func(t *testing.T) {
		d, _ := newDriver(t)

		assert.NoError(t, d.Stop(t.Context(), "", "nothing-here"))
	})
}

func TestDriver_Discard(t *testing.T) {
	t.Parallel()

	t.Run("removes both trees including the output a stop kept", func(t *testing.T) {
		d, root := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo working; sleep 300"))
		require.NoError(t, err)

		// Stopped first, so what is discarded is a workload that has already been
		// through the path which keeps its output. That is the state a delete finds.
		require.NoError(t, d.Stop(t.Context(), "", "example"))
		require.NoError(t, d.Discard(t.Context(), "", "example"))

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		assert.Empty(t, instances)

		for _, tree := range []string{"state", "workloads"} {
			_, err = os.Stat(filepath.Join(root, tree, testID))
			assert.Truef(t, os.IsNotExist(err), "the workload's %s directory outlived it", tree)
		}
	})

	t.Run("stops a running process before removing it", func(t *testing.T) {
		d, root := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300 & echo $! > child.pid; wait"))
		require.NoError(t, err)

		child := awaitChildPID(t, root, "example", 1)

		require.NoError(t, d.Discard(t.Context(), "", "example"))

		assert.Eventually(t, func() bool {
			return syscall.Kill(child, 0) != nil
		}, 10*time.Second, 50*time.Millisecond, "a process the command started outlived the workload")
	})

	t.Run("does nothing for a workload it has never run", func(t *testing.T) {
		d, _ := newDriver(t)

		assert.NoError(t, d.Discard(t.Context(), "", "nothing-here"))
	})
}

func TestDriver_Signal(t *testing.T) {
	t.Parallel()

	t.Run("signals the process the workload runs", func(t *testing.T) {
		d, root := newDriver(t)

		// The script records that it was signalled, which is the only way to prove the
		// signal arrived at the process rather than merely being sent somewhere. It
		// announces its trap first: a signal is only handled once a handler exists, and
		// SIGHUP before then ends the process rather than being caught.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one",
			`trap 'echo reloaded > reloaded.txt' HUP; touch trapped.txt; while true; do sleep 0.05; done`))
		require.NoError(t, err)

		// The process loops until something stops it, and nothing here does. Without
		// this the test leaves it running after the test binary has gone, and every run
		// leaves another.
		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		cwd := filepath.Join(root, "workloads", testID, "0", "1", "cwd")

		require.Eventually(t, func() bool {
			_, err := os.Stat(filepath.Join(cwd, "trapped.txt"))

			return err == nil
		}, 10*time.Second, 50*time.Millisecond, "the process never installed its handler")

		require.NoError(t, d.Signal(t.Context(), testID, "example", "SIGHUP"))

		require.Eventually(t, func() bool {
			_, err := os.Stat(filepath.Join(cwd, "reloaded.txt"))

			return err == nil
		}, 10*time.Second, 50*time.Millisecond, "the process was never signalled")

		// Still running. A reload is not a stop, which is the whole point of naming
		// one.
		awaitState(t, d, "example", driver.StateRunning)
	})

	t.Run("leaves what the command started alone", func(t *testing.T) {
		d, root := newDriver(t)

		// Stopping a workload signals the group, so that whatever the command started
		// goes with it. A reload is for the program that read the file, so a child that
		// never asked for one must not be signalled.
		//
		// The parent installs a handler rather than ignoring the signal, which is what
		// makes this test able to fail: an ignored disposition is inherited across a
		// fork, so a child of a shell that ignored SIGHUP would survive a signal to the
		// whole group and prove nothing. A handler is not inherited, so this child takes
		// the default action and dies if the group is signalled.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one",
			`trap 'echo reloaded > reloaded.txt' HUP; sleep 300 & echo $! > child.pid; `+
				`touch trapped.txt; while true; do sleep 0.05; done`))
		require.NoError(t, err)

		// Discarding signals the group, so the child this test starts goes with the
		// parent. Both outlive the test binary otherwise.
		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		cwd := filepath.Join(root, "workloads", testID, "0", "1", "cwd")
		child := awaitChildPID(t, root, "example", 1)

		require.Eventually(t, func() bool {
			_, err := os.Stat(filepath.Join(cwd, "trapped.txt"))

			return err == nil
		}, 10*time.Second, 50*time.Millisecond, "the process never installed its handler")

		require.NoError(t, d.Signal(t.Context(), testID, "example", "SIGHUP"))

		// The workload itself was told, so the signal did arrive somewhere.
		require.Eventually(t, func() bool {
			_, err := os.Stat(filepath.Join(cwd, "reloaded.txt"))

			return err == nil
		}, 10*time.Second, 50*time.Millisecond, "the process was never signalled")

		assert.Never(t, func() bool {
			return syscall.Kill(child, 0) != nil
		}, time.Second, 100*time.Millisecond, "a process the command started was signalled")
	})

	t.Run("refuses a signal it does not send", func(t *testing.T) {
		d, _ := newDriver(t)

		// Whether a workload runs is the reconciler's decision, so a driver that could
		// be asked to kill a process would be taking it.
		for _, signal := range []string{"SIGKILL", "SIGTERM", "SIGINT", "HUP", "1", ""} {
			err := d.Signal(t.Context(), testID, "example", signal)
			assert.ErrorIs(t, err, exec.ErrUnknownSignal, "accepted the signal %q", signal)
		}
	})

	t.Run("does nothing for a workload it has never run", func(t *testing.T) {
		d, _ := newDriver(t)

		// A workload of another runtime looks like this from here.
		assert.NoError(t, d.Signal(t.Context(), "", "example", "SIGHUP"))
	})

	t.Run("does nothing for a workload that has ended", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "exit 0"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		// There is no process left to reload, and the pid may since have been reused by
		// something that has nothing to do with takt.
		assert.NoError(t, d.Signal(t.Context(), testID, "example", "SIGHUP"))
	})
}

func TestDriver_Stop_ResolvesAnOrphanByItsRecordedName(t *testing.T) {
	t.Parallel()

	// An orphan has no stored workload, so no identifier comes with it. The name is
	// found by reading the records rather than by treating it as a path, which is what
	// lets directories be named for the identifier.
	d, root := newDriver(t)

	pid := orphan(t)

	writeInstance(t, root, "example", 1, map[string]any{
		"pid":        pid,
		"startTicks": startTicks(t, pid),
		"specHash":   "hash-one",
		"version":    1,
		"startedAt":  time.Now().Format(time.RFC3339Nano),
	})

	// Discarded rather than stopped, which is what the reconciler does to an orphan:
	// nothing asked for the work, so there is nobody left to read its output.
	require.NoError(t, d.Discard(t.Context(), "", "example"))

	instances, err := d.Observe(t.Context())
	require.NoError(t, err)
	assert.Empty(t, instances, "the orphan was not stopped")
}

func TestDriver_Stop_IgnoresAWorkloadItDoesNotRun(t *testing.T) {
	t.Parallel()

	// A name reaches this driver for every orphan, whichever runtime owns it. One it
	// has no record of is not its work, so there is nothing to do — including for a
	// name that could never be a path.
	d, _ := newDriver(t)

	for _, name := range []string{"never-heard-of-it", "../outside", "..", "a/../../etc"} {
		assert.NoError(t, d.Stop(t.Context(), "", name), "refused a workload it simply does not run: %q", name)
	}
}

func TestDriver_RefusesAnIdentifierThatIsNotADirectory(t *testing.T) {
	t.Parallel()

	// Directories are named for the identifier, so that is the value which has to be
	// safe as a path component. A driver that removes directories should not build a
	// path from something it has not looked at.
	base := t.TempDir()

	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600))

	d := exec.New(exec.Config{
		Logger: slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError})),
		Root:   filepath.Join(base, "root"),
	})

	// An empty identifier is not in this list: Stop treats it as an orphan, which is
	// resolved by name instead and covered separately.
	for _, id := range []string{"../outside", "..", "../../etc", ".", "short", "UPPERCASE0000000000A"} {
		err := d.Stop(t.Context(), id, "example")
		assert.ErrorIs(t, err, exec.ErrInvalidWorkloadID, "accepted the identifier %q", id)

		w := workload("example", 1, "hash-one", "exit 0")
		w.ID = id

		_, err = d.Start(t.Context(), w)
		assert.ErrorIs(t, err, exec.ErrInvalidWorkloadID, "accepted the identifier %q", id)
	}

	// Start refuses an empty identifier as well, since a workload it is asked to run
	// always has one.
	empty := workload("example", 1, "hash-one", "exit 0")
	empty.ID = ""

	_, err := d.Start(t.Context(), empty)
	assert.ErrorIs(t, err, exec.ErrInvalidWorkloadID)

	_, statErr := os.Stat(filepath.Join(outside, "keep"))
	assert.NoError(t, statErr, "an identifier resolved outside the driver's root")
}

func TestDriver_RefusesToSignalPidZeroOrOne(t *testing.T) {
	t.Parallel()

	// A signal is sent to the negated pid, which addresses a process group. Zero
	// addresses the driver's own group and one addresses every process it may signal
	// at all, so neither can be work the driver started.
	//
	// The record a pid comes from lives where a workload can reach it, so this is what
	// remains if the identity check on that record ever does not hold.
	d, root := newDriver(t)

	for _, pid := range []int{0, 1, -1} {
		writeInstance(t, root, "example", 1, map[string]any{
			"pid":        pid,
			"startTicks": 1,
			"specHash":   "hash-one",
			"version":    1,
			"startedAt":  time.Now().Format(time.RFC3339Nano),
		})

		// Stop reports the record as not alive, so terminate is not reached. The
		// driver still must not treat such a pid as signalable at all.
		require.NoError(t, d.Stop(t.Context(), "", "example"))
	}
}

func TestDriver_Logs(t *testing.T) {
	t.Parallel()

	t.Run("combines both streams in the order they were written", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", `echo first; echo second 1>&2; echo third`))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 10}))
		assert.Equal(t, "first\nsecond\nthird\n", out.String())
	})

	t.Run("returns only the lines asked for", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "seq 1 100"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 3}))
		assert.Equal(t, "98\n99\n100\n", out.String())
	})

	t.Run("writes nothing for a workload it has never run", func(t *testing.T) {
		d, _ := newDriver(t)

		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "nothing-here", driver.LogOptions{Tail: 10}))
		assert.Empty(t, out.String())
	})

	t.Run("separates the current attempt from the one it replaced", func(t *testing.T) {
		d, _ := newDriver(t)

		// The first attempt, then the replacement, which is what the reconciler does to
		// a workload whose specification changed or whose process died.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo first attempt; sleep 300"))
		require.NoError(t, err)

		awaitOutput(t, d, "example", "first attempt")

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		_, err = d.Start(t.Context(), workload("example", 2, "hash-two", "echo second attempt; sleep 300"))
		require.NoError(t, err)

		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		var previous bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &previous, "example", driver.LogOptions{Tail: 10, Previous: true}))

		// The attempt that ended, which for a crash-looping workload is the one that
		// failed. Without retention this was gone by the time anything could read it.
		assert.Contains(t, previous.String(), "first attempt")
		assert.NotContains(t, previous.String(), "second attempt")

		var current bytes.Buffer
		require.Eventually(t, func() bool {
			current.Reset()
			require.NoError(t, d.Logs(t.Context(), &current, "example", driver.LogOptions{Tail: 10}))

			return strings.Contains(current.String(), "second attempt")
		}, 10*time.Second, 50*time.Millisecond, "the current attempt's output never arrived")

		// Never both. Concatenating them would return two runs spliced together with
		// nothing marking the boundary.
		assert.NotContains(t, current.String(), "first attempt")
	})

	t.Run("separates two attempts at the same version", func(t *testing.T) {
		d, _ := newDriver(t)

		// A crash-looping workload restarts at an unchanged version, so both attempts
		// run in the same directory. Nothing distinguishes them but the file each one's
		// output ends up in, which is what makes this the case retention gets wrong most
		// easily: appending to one file would leave nothing able to say where the first
		// attempt ended and the second began.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo first attempt; sleep 300"))
		require.NoError(t, err)

		awaitOutput(t, d, "example", "first attempt")

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		_, err = d.Start(t.Context(), workload("example", 1, "hash-one", "echo second attempt; sleep 300"))
		require.NoError(t, err)

		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		awaitOutput(t, d, "example", "second attempt")

		var current bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &current, "example", driver.LogOptions{Tail: 10}))
		assert.NotContains(t, current.String(), "first attempt")

		var previous bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &previous, "example", driver.LogOptions{Tail: 10, Previous: true}))
		assert.Contains(t, previous.String(), "first attempt")
		assert.NotContains(t, previous.String(), "second attempt")
	})

	t.Run("reports a restart at the same version as running", func(t *testing.T) {
		d, _ := newDriver(t)

		// The record for the replaced attempt is marked kept, and the replacement reuses
		// its directory. A mark left in place would report the running process as one
		// held only for its output, and the reconciler would never see it running.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
		require.NoError(t, err)

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		_, err = d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
		require.NoError(t, err)

		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, instances, 1)

		assert.False(t, instances[0].Retained, "a running process was reported as kept for its output")
		assert.Equal(t, driver.StateRunning, instances[0].State)
	})

	t.Run("writes nothing for a previous attempt that does not exist", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo only attempt"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		// A workload that has only ever run once has no earlier attempt, and saying so
		// beats falling back to the current one under the wrong heading.
		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 10, Previous: true}))
		assert.Empty(t, out.String())
	})
}

func TestDriver_Logs_Follow(t *testing.T) {
	t.Parallel()

	t.Run("writes output the workload produces after the read began", func(t *testing.T) {
		d, _ := newDriver(t)

		// The second line is written a second in, so the tail the follow opens with
		// cannot hold it. Anything that arrives is proof the follow is doing the work.
		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo started; sleep 1; echo later; sleep 300"))
		require.NoError(t, err)

		t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

		awaitOutput(t, d, "example", "started")

		ctx, cancel := context.WithCancel(t.Context())

		var out syncBuffer
		done := make(chan error, 1)

		go func() {
			done <- d.Logs(ctx, &out, "example", driver.LogOptions{Tail: 10, Follow: true})
		}()

		require.Eventually(t, func() bool {
			return strings.Contains(out.String(), "later")
		}, 10*time.Second, 50*time.Millisecond, "the followed output never arrived")

		// A caller pressing Ctrl-C is how most follows end, and it ends the read rather
		// than failing it: there is nobody left to report a failure to.
		cancel()

		select {
		case err = <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the follow outlived the caller that asked for it")
		}
	})

	t.Run("ends the read when the process ends", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo started; sleep 1; echo done"))
		require.NoError(t, err)

		// Returns of its own accord. What the caller asked to watch has finished, so
		// there is nothing further for the follow to wait on.
		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", driver.LogOptions{Tail: 10, Follow: true}))

		// Including the lines written on the way out. The record is read before the
		// file, so a process that ends mid-poll still has its last output returned.
		assert.Contains(t, out.String(), "done")
	})
}

func TestDriver_Watch(t *testing.T) {
	t.Parallel()

	d, _ := newDriver(t)

	events, err := d.Watch(t.Context())
	require.NoError(t, err)

	_, err = d.Start(t.Context(), workload("example", 1, "hash-one", "exit 0"))
	require.NoError(t, err)

	// A process ending is what the driver has instead of an event stream, and is what
	// lets the reconciler converge without waiting for its next tick.
	select {
	case event := <-events:
		assert.Equal(t, "example", event.Workload)
	case <-time.After(10 * time.Second):
		t.Fatal("the driver reported no event for a process that ended")
	}
}

func TestDriver_Release(t *testing.T) {
	t.Parallel()

	d, _ := newDriver(t)

	_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Discard(context.Background(), "", "example") })

	instances, err := d.Observe(t.Context())
	require.NoError(t, err)
	require.Len(t, instances, 1)

	pid, err := strconv.Atoi(instances[0].ID)
	require.NoError(t, err)

	// Releasing is what makes a long-running workload outlive the server: the driver
	// gives up its claim on the process rather than stopping it.
	d.Release()

	assert.NoError(t, syscall.Kill(pid, 0), "a released process should still be running")

	instances, err = d.Observe(t.Context())
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, driver.StateRunning, instances[0].State)

	// The claim is what matters. A driver still holding one would reap the process
	// when it ended and, in a server that has exited, take it down with itself — so
	// the released process must no longer be supervised.
	assert.False(t, d.Supervises("example"), "a released process is still claimed by the driver")
}

// newDriver returns a driver rooted in a temporary directory, along with that root.
func newDriver(t *testing.T, options ...option) (*exec.Driver, string) {
	t.Helper()

	root := t.TempDir()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	config := exec.Config{
		Logger: slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: level})),
		Root:   root,
	}

	for _, option := range options {
		option(&config)
	}

	d := exec.New(config)

	// Registered before any test's own cleanup, so it runs after them: a test that
	// stops its workload has done so by the time this waits.
	//
	// A supervising goroutine records how its process ended and logs it. Both outlive
	// the call that started the process, and both touch things the end of a test takes
	// away — the logger writes to t.Output(), which panics once the test has finished,
	// and the record is written into the temp directory cleanup is removing. Either one
	// fails the test that happens to be running rather than the one that leaked.
	t.Cleanup(func() {
		settled := make(chan struct{})

		go func() {
			d.Wait()
			close(settled)
		}()

		select {
		case <-settled:
		case <-time.After(30 * time.Second):
			// A process the test left running, which nothing here can stop without
			// guessing at what the test meant. Named rather than waited out, since the
			// alternative is a suite that hangs.
			t.Error("the driver's supervising goroutines outlived the test")
		}
	})

	return d, root
}

// The option type modifies how a test's driver is configured.
type option func(*exec.Config)

// withAllowedPaths grants every workload the driver runs read-only access to the given
// paths, standing in for what an operator put in the server's configuration.
func withAllowedPaths(paths ...string) option {
	return func(c *exec.Config) { c.AllowPaths = paths }
}

// The identifier the tests use for their workload. Directories are named for the
// identifier rather than the name, so a test that looks on disk looks here.
const testID = "cvhs0dq0kqj4c9r8m1a0"

// workload returns a workload running the given shell script, which is the smallest
// way to get a command that does something observable.
func workload(name string, version int, hash, script string) driver.Workload {
	return driver.Workload{
		ID:       testID,
		Name:     name,
		Version:  version,
		SpecHash: hash,
		Spec: manifest.Spec{
			Version: "v1",
			Name:    name,
			Exec:    &manifest.Exec{Command: []string{"sh", "-c", script}},
		},
	}
}

func TestDriver_Instances(t *testing.T) {
	t.Parallel()

	t.Run("runs each instance in a directory and record of its own", func(t *testing.T) {
		d, root := newDriver(t)
		ctx := t.Context()

		first := workload("example", 1, "hash", "sleep 60")

		second := workload("example", 1, "hash", "sleep 60")
		second.Instance = 1

		_, err := d.Start(ctx, first)
		require.NoError(t, err)

		_, err = d.Start(ctx, second)
		require.NoError(t, err)

		defer func() { require.NoError(t, d.Discard(context.Background(), testID, "example")) }()

		// Two instances are two currents: neither replaces the other, so neither
		// reads as retained.
		instances, err := d.ObserveWorkload(ctx, testID, "example")
		require.NoError(t, err)
		require.Len(t, instances, 2)

		byIndex := make(map[int]driver.Instance, len(instances))
		for _, instance := range instances {
			byIndex[instance.Index] = instance
		}

		assert.Equal(t, driver.StateRunning, byIndex[0].State)
		assert.Equal(t, driver.StateRunning, byIndex[1].State)
		assert.False(t, byIndex[0].Retained)
		assert.False(t, byIndex[1].Retained)

		for _, instance := range []string{"0", "1"} {
			_, err = os.Stat(filepath.Join(root, "workloads", testID, instance, "1", "cwd"))
			assert.NoError(t, err)
		}

		// Stopping one instance leaves the other running.
		require.NoError(t, d.StopInstance(ctx, testID, "example", 1))

		require.Eventually(t, func() bool {
			instances, err = d.ObserveWorkload(ctx, testID, "example")
			if err != nil || len(instances) != 2 {
				return false
			}

			for _, instance := range instances {
				byIndex[instance.Index] = instance
			}

			return byIndex[1].Retained && byIndex[0].State == driver.StateRunning
		}, 10*time.Second, 50*time.Millisecond, "the stopped instance was never retained")
	})

	t.Run("discarding an instance removes what a stop retained for it", func(t *testing.T) {
		d, root := newDriver(t)
		ctx := t.Context()

		first := workload("example", 1, "hash", "sleep 60")

		second := workload("example", 1, "hash", "sleep 60")
		second.Instance = 1

		_, err := d.Start(ctx, first)
		require.NoError(t, err)

		_, err = d.Start(ctx, second)
		require.NoError(t, err)

		defer func() { require.NoError(t, d.Discard(context.Background(), testID, "example")) }()

		require.NoError(t, d.DiscardInstance(ctx, testID, "example", 1))

		instances, err := d.ObserveWorkload(ctx, testID, "example")
		require.NoError(t, err)
		require.Len(t, instances, 1)
		assert.Equal(t, 0, instances[0].Index)

		_, err = os.Stat(filepath.Join(root, "state", testID, "1"))
		assert.True(t, os.IsNotExist(err), "the discarded instance's records remain")
	})
}

// awaitState waits for a workload's single instance to reach the given state.
func awaitState(t *testing.T, d *exec.Driver, workload string, want driver.State) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		instances, err := d.Observe(context.Background())
		if err != nil || len(instances) != 1 {
			return false
		}

		return instances[0].Workload == workload && instances[0].State == want
	}, 10*time.Second, 50*time.Millisecond, "workload %q never reached %q", workload, want)
}

// awaitOutput waits until a workload has written the given text.
//
// A process is started asynchronously, so anything asserting about what it wrote has to
// wait for it. Stopping a workload the instant it starts otherwise races the command's
// first write, and a retained output file that is merely empty proves nothing.
func awaitOutput(t *testing.T, d *exec.Driver, workload, want string) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		var out bytes.Buffer
		if err := d.Logs(t.Context(), &out, workload, driver.LogOptions{Tail: 100}); err != nil {
			return false
		}

		return strings.Contains(out.String(), want)
	}, 10*time.Second, 50*time.Millisecond, "workload %q never wrote %q", workload, want)
}

// The syncBuffer type collects what a follow writes while the test reads it, since the
// two happen on different goroutines.
type syncBuffer struct {
	mux sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	return b.buf.String()
}

// output returns everything a workload has written, which is where a denial from the
// kernel lands: the command's own stderr.
func output(t *testing.T, d *exec.Driver, workload string) string {
	t.Helper()

	var out bytes.Buffer
	require.NoError(t, d.Logs(t.Context(), &out, workload, driver.LogOptions{Tail: 100}))

	return out.String()
}

// awaitChildPID reads the pid a command wrote into its working directory, which is how
// a test learns about a process the driver never knew about directly.
func awaitChildPID(t *testing.T, root, workload string, version int) int {
	t.Helper()

	path := filepath.Join(root, "workloads", testID, "0", strconv.Itoa(version), "cwd", "child.pid")

	var pid int

	require.Eventuallyf(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}

		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))

		return err == nil && pid > 0
	}, 10*time.Second, 50*time.Millisecond, "the command never reported its child")

	return pid
}

// writeInstance writes a record for a workload the driver did not start, which is how
// a test stands in for a process adopted from an earlier server.
func writeInstance(t *testing.T, root, workload string, version int, recorded map[string]any) {
	t.Helper()

	// Records live in the driver's own tree, which no workload runs inside, in a
	// directory named for the identifier. The name is inside the record.
	recorded["workload"] = workload

	dir := filepath.Join(root, "state", testID, "0", strconv.Itoa(version))
	require.NoError(t, os.MkdirAll(dir, 0o700))

	data, err := json.Marshal(recorded)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600))
}

// writeInstanceFor writes a record under a chosen identifier, for a test that needs
// more than one workload in the tree. writeInstance keys everything on testID.
func writeInstanceFor(t *testing.T, root, id, workload string, version int, recorded map[string]any) {
	t.Helper()

	recorded["workload"] = workload

	dir := filepath.Join(root, "state", id, "0", strconv.Itoa(version))
	require.NoError(t, os.MkdirAll(dir, 0o700))

	data, err := json.Marshal(recorded)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600))
}

// orphan starts a process with no supervisor and returns its pid, standing in for one
// that outlived the server that started it.
func orphan(t *testing.T) int {
	t.Helper()

	cmd := osexec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start())

	pid := cmd.Process.Pid
	require.NoError(t, cmd.Process.Release())

	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	return pid
}

// startTicks reads when a process started, which is what distinguishes it from a later
// process that reuses its pid.
func startTicks(t *testing.T, pid int) uint64 {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	require.NoError(t, err)

	line := string(data)
	fields := strings.Fields(line[strings.LastIndex(line, ")")+2:])

	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	require.NoError(t, err)

	return ticks
}

// freePID returns a pid nothing holds, so a record naming it describes a process that
// is gone.
func freePID(t *testing.T) int {
	t.Helper()

	cmd := osexec.Command("true")
	require.NoError(t, cmd.Run())

	return cmd.Process.Pid
}

func TestDriver_Start_MountsVolumes(t *testing.T) {
	t.Parallel()

	t.Run("places a volume at the path the workload names", func(t *testing.T) {
		t.Parallel()

		// The workload reaches its volume at the path from its own manifest, resolved
		// against the working directory it starts in. Nothing tells it where the
		// volume really is.
		d, root := newDriver(t)
		volume := newVolume(t, "example-data")

		// The command writes through a relative path. The working directory is where
		// the volume was placed, and an absolute path in a command reaches the host's
		// root rather than the workload's.
		w := workload("example", 1, "hash-one", "echo written > var/lib/example/file")
		w.Volumes = []driver.Volume{{Name: "example-data", Host: volume, Target: "/var/lib/example"}}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		// The file landed in the volume rather than in the working directory.
		contents, err := os.ReadFile(filepath.Join(volume, "file"))
		require.NoError(t, err)
		assert.Equal(t, "written\n", string(contents))

		link := filepath.Join(root, "workloads", testID, "0", "1", "cwd", "var", "lib", "example")
		target, err := os.Readlink(link)
		require.NoError(t, err, "the volume was not linked into the working directory")
		assert.Equal(t, volume, target)
	})

	t.Run("creates the directories leading to a nested path", func(t *testing.T) {
		t.Parallel()

		d, root := newDriver(t)
		volume := newVolume(t, "example-data")

		w := workload("example", 1, "hash-one", "exit 0")
		w.Volumes = []driver.Volume{{Name: "example-data", Host: volume, Target: "/a/b/c/deep"}}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		_, err = os.Lstat(filepath.Join(root, "workloads", testID, "0", "1", "cwd", "a", "b", "c", "deep"))
		assert.NoError(t, err)
	})

	t.Run("keeps a volume inside the working directory", func(t *testing.T) {
		t.Parallel()

		// The target comes from a specification submitted over the API. A path
		// climbing out of the working directory is resolved against it rather than
		// escaping, so a manifest cannot direct takt to write elsewhere on the host.
		d, root := newDriver(t)
		volume := newVolume(t, "example-data")

		cwd := filepath.Join(root, "workloads", testID, "0", "1", "cwd")

		for _, target := range []string{"/../../escape", "../../escape", "/./x", "/a/../b"} {
			w := workload("example", 1, "hash-one", "exit 0")
			w.Volumes = []driver.Volume{{Name: "example-data", Host: volume, Target: target}}

			_, err := d.Start(t.Context(), w)
			require.NoError(t, err, "refused the target %q", target)

			// Everything the target produced is inside the working directory, so
			// nothing was written above it.
			entries, err := os.ReadDir(cwd)
			require.NoError(t, err)
			assert.NotEmpty(t, entries, "the target %q placed nothing", target)

			// Waited for before stopping. The driver records an exit from a goroutine
			// of its own, and removing the directory it writes into while it is still
			// running leaves it logging into a finished subtest.
			awaitState(t, d, "example", driver.StateExited)

			require.NoError(t, d.Stop(t.Context(), testID, "example"))

			// The volume itself survived, wherever the target pointed.
			_, err = os.Stat(volume)
			assert.NoError(t, err, "the target %q destroyed the volume", target)
		}
	})
}

func TestDriver_Start_ConfinesTheWorkload(t *testing.T) {
	t.Parallel()

	t.Run("refuses a path the workload was never granted", func(t *testing.T) {
		// The reason this exists. An exec workload runs as the server's own uid, so
		// nothing about file ownership stops it reading the database, the encryption key
		// or another workload's mounted plaintext. The kernel is what does.
		d, _ := newDriver(t)

		secret := filepath.Join(t.TempDir(), "secret.key")
		require.NoError(t, os.WriteFile(secret, []byte("SEALED"), 0o600))

		// Readable by whoever runs the tests, so an unconfined command would print it.
		contents, err := os.ReadFile(secret)
		require.NoError(t, err)
		require.Equal(t, "SEALED", string(contents), "this test needs a file the user can read")

		_, err = d.Start(t.Context(), workload("example", 1, "hash-one", "cat "+secret+" 2>&1; exit 0"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		out := output(t, d, "example")
		assert.NotContains(t, out, "SEALED", "a confined workload read a file it was never granted")
		assert.Contains(t, out, "Permission denied")
	})

	t.Run("allows the working directory it was given", func(t *testing.T) {
		d, root := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "echo written > file"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		contents, err := os.ReadFile(filepath.Join(root, "workloads", testID, "0", "1", "cwd", "file"))
		require.NoError(t, err, "a confined workload could not write its own working directory")
		assert.Equal(t, "written\n", string(contents))
	})

	t.Run("allows a volume it mounts", func(t *testing.T) {
		// A volume lives outside the working directory and is reached through a link
		// into it. The kernel resolves the link before deciding, so the volume has to be
		// granted where it really is or the workload cannot write what it asked for.
		d, _ := newDriver(t)
		volume := newVolume(t, "example-data")

		w := workload("example", 1, "hash-one", "echo written > var/lib/example/file")
		w.Volumes = []driver.Volume{{Name: "example-data", Host: volume, Target: "/var/lib/example"}}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		contents, err := os.ReadFile(filepath.Join(volume, "file"))
		require.NoError(t, err, "a confined workload could not write a volume it mounts")
		assert.Equal(t, "written\n", string(contents))
	})

	t.Run("allows a mounted value to be read, and no other in its directory", func(t *testing.T) {
		// Mounted values all live in one tree, one directory per workload version. A
		// ruleset granting the directory rather than the file would hand a workload
		// every other workload's secrets, which is the failure this pins.
		d, _ := newDriver(t)

		dir := t.TempDir()

		mine := filepath.Join(dir, "secret-mine")
		require.NoError(t, os.WriteFile(mine, []byte("MINE"), 0o444))

		theirs := filepath.Join(dir, "secret-theirs")
		require.NoError(t, os.WriteFile(theirs, []byte("THEIRS"), 0o444))

		w := workload("example", 1, "hash-one", "cat token 2>&1; cat "+theirs+" 2>&1; exit 0")
		w.Volumes = []driver.Volume{{Name: "secret-mine", Host: mine, Target: "/token"}}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		out := output(t, d, "example")
		assert.Contains(t, out, "MINE", "a confined workload could not read the value it mounts")
		assert.NotContains(t, out, "THEIRS", "a confined workload read a value another workload mounts")
	})

	t.Run("cannot write a value it mounts", func(t *testing.T) {
		// A mounted value is granted read-only, and from the third Landlock ABI that
		// covers truncation too. Below it a workload could empty a value it cannot
		// rewrite, which is why that ABI is the floor.
		d, _ := newDriver(t)

		value := filepath.Join(t.TempDir(), "secret-token")
		require.NoError(t, os.WriteFile(value, []byte("MOUNTED"), 0o444))

		// Both attempts run in a subshell: a redirection the kernel refuses ends the
		// shell that tried it, and the second attempt is the one this test is about.
		w := workload("example", 1, "hash-one",
			"(echo overwrite > token) 2>&1; (: > token) 2>&1; exit 0")
		w.Volumes = []driver.Volume{{Name: "secret-token", Host: value, Target: "/token"}}

		_, err := d.Start(t.Context(), w)
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		contents, err := os.ReadFile(value)
		require.NoError(t, err)
		assert.Equal(t, "MOUNTED", string(contents), "a confined workload changed a value it mounts")
	})

	t.Run("refuses another process's environment", func(t *testing.T) {
		// An exec workload's environment is readable at /proc/<pid>/environ by the user
		// running it, and every exec workload runs as that same user. Without
		// confinement one workload can therefore read another's secrets straight out of
		// it. This is what closes that.
		d, _ := newDriver(t)

		pid := orphan(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one",
			"cat /proc/"+strconv.Itoa(pid)+"/environ 2>&1; exit 0"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		assert.Contains(t, output(t, d, "example"), "Permission denied")
	})

	t.Run("allows the host's own files, so an ordinary command runs", func(t *testing.T) {
		// A dynamically linked program needs its interpreter and its libraries, and the
		// tests above would all pass against a ruleset so tight that nothing ran at all.
		// This is what says confinement is strict rather than useless.
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one",
			`cat /etc/hostname > /dev/null && echo read-etc; echo discarded > /dev/null && echo wrote-devnull`))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		out := output(t, d, "example")
		assert.Contains(t, out, "read-etc")
		assert.Contains(t, out, "wrote-devnull")
	})

	t.Run("allows a path the operator granted", func(t *testing.T) {
		// The escape hatch, for a runtime that lives somewhere the host's own
		// directories do not cover. Read-only, and the operator's decision rather than
		// the workload's.
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "runtime"), []byte("GRANTED"), 0o444))

		d, _ := newDriver(t, withAllowedPaths(dir))

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one",
			"cat "+filepath.Join(dir, "runtime")+" 2>&1; echo denied > "+filepath.Join(dir, "written")+" 2>&1; exit 0"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		out := output(t, d, "example")
		assert.Contains(t, out, "GRANTED", "a workload could not read a path the operator granted")

		// Granted for reading only, so a path the operator opened is not one a workload
		// can write.
		_, err = os.Stat(filepath.Join(dir, "written"))
		assert.True(t, os.IsNotExist(err), "a workload wrote a path granted only for reading")
	})

	t.Run("refuses a command that is not there, naming it", func(t *testing.T) {
		// Reported as a failure to start rather than as an exit code, because a command
		// that cannot be found is not a workload that ran and failed.
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "exit 0"))
		require.NoError(t, err)

		w := workload("missing", 1, "hash-one", "exit 0")
		w.Spec.Exec.Command = []string{"definitely-not-a-command-on-this-host"}

		_, err = d.Start(t.Context(), w)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "definitely-not-a-command-on-this-host")
	})
}

func TestDriver_Stop_LeavesVolumeContentsAlone(t *testing.T) {
	t.Parallel()

	// The whole point of a volume. Stopping a workload removes the tree its working
	// directory sits in, and the reconciler stops a workload before every replacement
	// and every retry, so a volume that did not survive this would be no better than
	// the working directory it replaced.
	d, _ := newDriver(t)
	volume := newVolume(t, "example-data")

	w := workload("example", 1, "hash-one", "echo precious > var/lib/example/file")
	w.Volumes = []driver.Volume{{Name: "example-data", Host: volume, Target: "/var/lib/example"}}

	_, err := d.Start(t.Context(), w)
	require.NoError(t, err)

	awaitState(t, d, "example", driver.StateExited)

	require.NoError(t, d.Stop(t.Context(), testID, "example"))

	contents, err := os.ReadFile(filepath.Join(volume, "file"))
	require.NoError(t, err, "stopping the workload destroyed the volume's contents")
	assert.Equal(t, "precious\n", string(contents))
}

// newVolume returns a directory standing in for a volume, which lives outside the
// driver's root exactly as a real one does.
func newVolume(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "volumes", name)
	require.NoError(t, os.MkdirAll(path, 0o700))

	return path
}
