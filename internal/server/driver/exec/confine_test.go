//go:build linux

package exec_test

import (
	"encoding/json"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/dsb-labs/orca/internal/server/driver/exec"
)

func TestConfine(t *testing.T) {
	t.Parallel()

	t.Run("becomes the command the ruleset names", func(t *testing.T) {
		// The trampoline's whole purpose: it applies a ruleset to itself and then turns
		// into the workload's command. A process that reported success without becoming
		// the command would leave the driver supervising something that runs nothing.
		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "echo became-the-command"},
		})

		require.Empty(t, result.Status, "the trampoline refused to confine")
		assert.Equal(t, 0, result.ExitCode)
		assert.Contains(t, result.Output, "became-the-command")
	})

	t.Run("keeps the pid it was started with", func(t *testing.T) {
		// The record the driver writes names a pid, and the liveness check compares it
		// against what the kernel reports. Executing the command replaces the process
		// image rather than creating a process, so the pid the driver recorded is still
		// the command's — which is what lets adoption keep working unchanged.
		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", `echo "pid=$$"`},
		})

		require.Empty(t, result.Status)
		assert.Contains(t, result.Output, "pid="+strconv.Itoa(result.PID))
	})

	t.Run("reports the exit code of the command", func(t *testing.T) {
		// The driver decides whether a workload failed from this. A trampoline that
		// swallowed the code would have every workload look like a clean exit.
		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "exit 7"},
		})

		require.Empty(t, result.Status)
		assert.Equal(t, 7, result.ExitCode)
	})

	t.Run("reports a path the kernel refused, naming it", func(t *testing.T) {
		// A ruleset naming something that is not there is a bug in orca or a volume
		// that went missing. Either way the workload must not start, and the failure has
		// to say which path caused it — the alternative is an unexplained exit code.
		missing := filepath.Join(t.TempDir(), "not-here")

		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "echo should-not-run"},
			Write:   []string{missing},
		})

		require.NotEmpty(t, result.Status, "the trampoline confined a workload against a path that is not there")
		assert.Contains(t, result.Status, missing)
		assert.NotContains(t, result.Output, "should-not-run")
		assert.NotEqual(t, 0, result.ExitCode)
	})

	t.Run("reports a command that cannot be executed", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-command")

		result := trampoline(t, ruleset{Command: []string{missing}})

		require.NotEmpty(t, result.Status)
		assert.Contains(t, result.Status, missing)
		assert.NotEqual(t, 0, result.ExitCode)
	})

	t.Run("refuses a ruleset naming no command", func(t *testing.T) {
		result := trampoline(t, ruleset{})

		require.NotEmpty(t, result.Status)
		assert.NotEqual(t, 0, result.ExitCode)
	})

	t.Run("refuses everything the ruleset does not name", func(t *testing.T) {
		// The confinement itself, exercised at the lowest level there is: a file this
		// user can read, which the command cannot once the ruleset is applied.
		secret := filepath.Join(t.TempDir(), "secret.key")
		require.NoError(t, os.WriteFile(secret, []byte("SEALED"), 0o600))

		contents, err := os.ReadFile(secret)
		require.NoError(t, err)
		require.Equal(t, "SEALED", string(contents), "this test needs a file the user can read")

		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "cat " + secret + " 2>&1"},
		})

		require.Empty(t, result.Status)
		assert.NotContains(t, result.Output, "SEALED", "a confined command read a path the ruleset never named")
		assert.Contains(t, result.Output, "Permission denied")
	})

	t.Run("allows a directory the ruleset grants for writing", func(t *testing.T) {
		dir := t.TempDir()

		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "echo written > " + filepath.Join(dir, "file") + " 2>&1"},
			Write:   []string{dir},
		})

		require.Empty(t, result.Status)

		contents, err := os.ReadFile(filepath.Join(dir, "file"))
		require.NoError(t, err, "a confined command could not write a directory it was granted")
		assert.Equal(t, "written\n", string(contents))
	})

	t.Run("allows a file the ruleset grants for reading, and no other in its directory", func(t *testing.T) {
		// A mounted value lives beside every other workload's, so it is granted as a
		// file. Granting the directory instead would hand over all of them, which is
		// the failure this pins.
		dir := t.TempDir()

		mine := filepath.Join(dir, "secret-mine")
		require.NoError(t, os.WriteFile(mine, []byte("MINE"), 0o444))

		theirs := filepath.Join(dir, "secret-theirs")
		require.NoError(t, os.WriteFile(theirs, []byte("THEIRS"), 0o444))

		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "cat " + mine + " 2>&1; cat " + theirs + " 2>&1"},
			Files:   []string{mine},
		})

		require.Empty(t, result.Status)
		assert.Contains(t, result.Output, "MINE", "a confined command could not read the file it was granted")
		assert.NotContains(t, result.Output, "THEIRS", "a confined command read another file in the same directory")
	})

	t.Run("cannot write a file granted only for reading", func(t *testing.T) {
		// The reason the third ABI is the floor. Below it a read-only grant does not
		// cover truncation, so a workload could empty a value it cannot rewrite.
		dir := t.TempDir()

		value := filepath.Join(dir, "secret-value")
		require.NoError(t, os.WriteFile(value, []byte("MOUNTED"), 0o444))

		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "echo overwrite > " + value + " 2>&1; : > " + value + " 2>&1"},
			Files:   []string{value},
		})

		require.Empty(t, result.Status)

		contents, err := os.ReadFile(value)
		require.NoError(t, err)
		assert.Equal(t, "MOUNTED", string(contents), "a confined command changed a file granted only for reading")
	})

	t.Run("allows the host's own files, so an ordinary command runs", func(t *testing.T) {
		// A ruleset too tight to run a dynamically linked program at all would make
		// confinement useless rather than strict. This is what the built-in system
		// paths are for.
		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c",
				`cat /etc/hostname > /dev/null && echo read-etc; head -c 4 /dev/urandom > /dev/null && echo read-random`},
		})

		require.Empty(t, result.Status)
		assert.Contains(t, result.Output, "read-etc")
		assert.Contains(t, result.Output, "read-random")
	})

	t.Run("refuses another process's environment", func(t *testing.T) {
		// Every exec workload runs as the same user, so /proc/<pid>/environ is readable
		// between them without confinement. This is what closes it.
		pid := orphan(t)

		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", "cat /proc/" + strconv.Itoa(pid) + "/environ 2>&1"},
		})

		require.Empty(t, result.Status)
		assert.Contains(t, result.Output, "Permission denied")
	})

	t.Run("strips the ambient capabilities the server was granted", func(t *testing.T) {
		// An operator grants the server CAP_DAC_OVERRIDE so it can delete a volume a
		// container wrote as another user. An ambient capability survives an exec, and
		// every ruleset grants the host's own files for reading — so a command that
		// kept the grant could read past file permissions on all of them.
		attr := &syscall.SysProcAttr{Setsid: true}

		// The trampoline has to hold an ambient capability before the strip is
		// observable. One the test process already holds — from pam_cap or setpriv —
		// is inherited. Without one, the trampoline starts in a user namespace of its
		// own, where the test may raise one unprivileged: the capability is then
		// scoped to the namespace, but what the trampoline does with it is not.
		if !ambient(t) {
			if !namespaceable() {
				t.Skip("this host refuses unprivileged user namespaces, and the test process holds no ambient capability — see CONTRIBUTING.md")
			}

			attr.Cloneflags = syscall.CLONE_NEWUSER
			attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: nsID, HostID: os.Getuid(), Size: 1}}
			attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: nsID, HostID: os.Getgid(), Size: 1}}
			attr.AmbientCaps = []uintptr{unix.CAP_DAC_OVERRIDE}
		}

		result := trampolineAs(t, ruleset{
			Command: []string{"/bin/sh", "-c", "grep ^Cap /proc/self/status"},
		}, attr)

		require.Empty(t, result.Status)
		assert.Contains(t, result.Output, "CapAmb:\t0000000000000000")
		assert.Contains(t, result.Output, "CapEff:\t0000000000000000")
	})

	t.Run("keeps its own process group, so stopping it reaches its children", func(t *testing.T) {
		// The driver signals the group rather than the process, so that whatever the
		// command started stops with it. Executing through the trampoline must not
		// change which group the command lands in.
		result := trampoline(t, ruleset{
			Command: []string{"/bin/sh", "-c", `echo "pgid=$(ps -o pgid= -p $$ | tr -d " ")"`},
		})

		require.Empty(t, result.Status)
		assert.Contains(t, result.Output, "pgid="+strconv.Itoa(result.PID))
	})
}

func TestConfinable(t *testing.T) {
	t.Parallel()

	// Every test in this file confines something successfully, so this host offers what
	// is required and the answer here has to agree. A host that did not would fail the
	// rest of the file, which is what makes this worth asserting rather than skipping.
	assert.NoError(t, exec.Confinable(), "this host cannot confine a workload, so no other test here is meaningful")
}

type (
	// The ruleset type mirrors what the driver sends the trampoline. It is unexported
	// there, because nothing outside the package composes one — so the test states the
	// wire format it depends on rather than reaching for the internal type.
	ruleset struct {
		Command []string `json:"command"`
		Write   []string `json:"write"`
		Read    []string `json:"read"`
		Files   []string `json:"files"`
	}

	// The confined type is what running a trampoline produced.
	confined struct {
		// What the command wrote, both streams together, as the driver captures it.
		Output string
		// Why confinement failed, empty when the command ran.
		Status string
		// The exit code of the process, which is the command's own once it ran.
		ExitCode int
		// The pid the process was started with, which the command keeps.
		PID int
	}
)

// The uid the trampoline holds inside a user namespace of its own. Any value except
// zero: a process with uid zero regains every capability when it execs, so the strip
// would be invisible. Not zero also keeps the test meaningful when run as root.
const nsID = 1000

// namespaceable reports whether this process may create a user namespace it holds
// capabilities in, which is what lets the test grant an ambient capability without
// the machine's help. Ubuntu refuses this for unprivileged processes when
// kernel.apparmor_restrict_unprivileged_userns is set.
func namespaceable() bool {
	cmd := osexec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: nsID, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: nsID, HostID: os.Getgid(), Size: 1}},
	}

	return cmd.Run() == nil
}

// ambient reports whether this process holds any ambient capability, which is what
// the trampoline has to be seen stripping.
func ambient(t *testing.T) bool {
	t.Helper()

	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)

	for line := range strings.Lines(string(status)) {
		if rest, ok := strings.CutPrefix(line, "CapAmb:"); ok {
			held, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
			require.NoError(t, err)

			return held != 0
		}
	}

	return false
}

// trampoline runs the test binary as a confinement trampoline against the given
// ruleset, the same way the driver runs orca itself.
//
// The test binary is the binary under test, so this exercises the real code path: the
// ruleset goes in over a pipe, the reason it failed comes back over another, and
// everything the command writes lands in a file as it does for a real workload.
func trampoline(t *testing.T, rs ruleset) confined {
	t.Helper()

	return trampolineAs(t, rs, &syscall.SysProcAttr{Setsid: true})
}

// trampolineAs is trampoline with the process attributes the trampoline starts
// under, for the one test that starts it inside a user namespace.
func trampolineAs(t *testing.T, rs ruleset, attr *syscall.SysProcAttr) confined {
	t.Helper()

	self, err := os.Executable()
	require.NoError(t, err)

	in, ruleWriter, err := os.Pipe()
	require.NoError(t, err)

	statusReader, status, err := os.Pipe()
	require.NoError(t, err)

	// Opened before the command starts, as the driver opens a workload's output. A
	// descriptor already open stays usable after confinement, which is what lets a
	// workload write its log at all.
	output := filepath.Join(t.TempDir(), "output.log")

	out, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)

	cmd := osexec.Command(self, "__confine")
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{in, status}
	cmd.SysProcAttr = attr

	require.NoError(t, cmd.Start())

	// The parent's copies have no further use, and the status pipe would never reach
	// end of file while this one is still open.
	require.NoError(t, in.Close())
	require.NoError(t, status.Close())
	require.NoError(t, out.Close())

	require.NoError(t, json.NewEncoder(ruleWriter).Encode(rs))
	require.NoError(t, ruleWriter.Close())

	result := confined{PID: cmd.Process.Pid}

	// Read to end of file, which the exec produces by closing the writing end. A
	// command that is running therefore reports nothing here, and this does not wait
	// for it to finish.
	reason, err := io.ReadAll(statusReader)
	require.NoError(t, err)
	require.NoError(t, statusReader.Close())

	result.Status = strings.TrimSpace(string(reason))

	_ = cmd.Wait()
	result.ExitCode = cmd.ProcessState.ExitCode()

	written, err := os.ReadFile(output)
	require.NoError(t, err)

	result.Output = string(written)

	return result
}
