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
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/internal/server/driver"
	"github.com/dsb-labs/orca/internal/server/driver/exec"
	"github.com/dsb-labs/orca/internal/server/reconciler"
	"github.com/dsb-labs/orca/internal/server/service"
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
// A confined workload is started by orca executing itself, so the tests need the same
// of the binary they run in: started as a trampoline it has to confine itself and
// become the command, rather than run the suite a second time inside the workload.
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
		info, err := os.Stat(filepath.Join(root, "workloads", testID, "1"))
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
		require.NoError(t, d.Logs(t.Context(), &out, "example", 10))

		// HOME is absent, EXAMPLE is what the workload asked for, and PATH is supplied
		// so that a command named by anything but an absolute path can be found.
		assert.Contains(t, out.String(), "[][EXAMPLE][")
		assert.NotContains(t, out.String(), os.Getenv("HOME"))
	})
}

func TestDriver_Observe(t *testing.T) {
	t.Parallel()

	t.Run("reports a long-running process as running", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Stop(context.Background(), "", "example") })

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
		// that started it. Adoption is what makes restarting orca different from
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

	t.Run("stops a running process and removes its files", func(t *testing.T) {
		d, root := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "sleep 300"))
		require.NoError(t, err)

		require.NoError(t, d.Stop(t.Context(), "", "example"))

		instances, err := d.Observe(t.Context())
		require.NoError(t, err)
		assert.Empty(t, instances)

		for _, tree := range []string{"state", "workloads"} {
			_, err = os.Stat(filepath.Join(root, tree, testID))
			assert.True(t, os.IsNotExist(err), "the workload's %s directory outlived it", tree)
		}
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

		cwd := filepath.Join(root, "workloads", testID, "1", "cwd")

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

		cwd := filepath.Join(root, "workloads", testID, "1", "cwd")
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
		// something that has nothing to do with orca.
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

	require.NoError(t, d.Stop(t.Context(), "", "example"))

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
		require.NoError(t, d.Logs(t.Context(), &out, "example", 10))
		assert.Equal(t, "first\nsecond\nthird\n", out.String())
	})

	t.Run("returns only the lines asked for", func(t *testing.T) {
		d, _ := newDriver(t)

		_, err := d.Start(t.Context(), workload("example", 1, "hash-one", "seq 1 100"))
		require.NoError(t, err)

		awaitState(t, d, "example", driver.StateExited)

		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "example", 3))
		assert.Equal(t, "98\n99\n100\n", out.String())
	})

	t.Run("writes nothing for a workload it has never run", func(t *testing.T) {
		d, _ := newDriver(t)

		var out bytes.Buffer
		require.NoError(t, d.Logs(t.Context(), &out, "nothing-here", 10))
		assert.Empty(t, out.String())
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
	t.Cleanup(func() { _ = d.Stop(context.Background(), "", "example") })

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

	return exec.New(config), root
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
		Spec: api.WorkloadSpec{
			Version: "v1",
			Name:    name,
			Exec:    &api.ExecSpec{Command: []string{"sh", "-c", script}},
		},
	}
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

// output returns everything a workload has written, which is where a denial from the
// kernel lands: the command's own stderr.
func output(t *testing.T, d *exec.Driver, workload string) string {
	t.Helper()

	var out bytes.Buffer
	require.NoError(t, d.Logs(t.Context(), &out, workload, 100))

	return out.String()
}

// awaitChildPID reads the pid a command wrote into its working directory, which is how
// a test learns about a process the driver never knew about directly.
func awaitChildPID(t *testing.T, root, workload string, version int) int {
	t.Helper()

	path := filepath.Join(root, "workloads", testID, strconv.Itoa(version), "cwd", "child.pid")

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

	dir := filepath.Join(root, "state", testID, strconv.Itoa(version))
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

		link := filepath.Join(root, "workloads", testID, "1", "cwd", "var", "lib", "example")
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

		_, err = os.Lstat(filepath.Join(root, "workloads", testID, "1", "cwd", "a", "b", "c", "deep"))
		assert.NoError(t, err)
	})

	t.Run("keeps a volume inside the working directory", func(t *testing.T) {
		t.Parallel()

		// The target comes from a specification submitted over the API. A path
		// climbing out of the working directory is resolved against it rather than
		// escaping, so a manifest cannot direct orca to write elsewhere on the host.
		d, root := newDriver(t)
		volume := newVolume(t, "example-data")

		cwd := filepath.Join(root, "workloads", testID, "1", "cwd")

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

		contents, err := os.ReadFile(filepath.Join(root, "workloads", testID, "1", "cwd", "file"))
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
