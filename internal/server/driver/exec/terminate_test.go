//go:build linux

package exec

import (
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriver_Terminate(t *testing.T) {
	t.Parallel()

	t.Run("kills a child that outlived the leader", func(t *testing.T) {
		t.Parallel()

		// Without a cgroup the process group is all the driver has, and the leader
		// going is not the group going: a child that ignored the request kept the
		// workload's ports, so the next attempt could not bind them.
		leader, child := group(t)

		d := &Driver{logger: slog.New(slog.DiscardHandler)}
		require.NoError(t, d.terminate(t.Context(), leader))

		assert.Eventually(t, func() bool {
			return syscall.Kill(child, 0) != nil
		}, 10*time.Second, 50*time.Millisecond, "a child survived the group it was in being stopped")
	})
}

func TestInSession(t *testing.T) {
	t.Parallel()

	t.Run("finds a member of a session whose leader has gone", func(t *testing.T) {
		t.Parallel()

		leader, child := group(t)

		require.NoError(t, syscall.Kill(leader, syscall.SIGTERM))
		require.Eventually(t, func() bool {
			return syscall.Kill(leader, 0) != nil
		}, 10*time.Second, 50*time.Millisecond)

		assert.True(t, inSession(leader), "the child was not found in its leader's session")

		require.NoError(t, syscall.Kill(child, syscall.SIGKILL))
		assert.Eventually(t, func() bool {
			return !inSession(leader)
		}, 10*time.Second, 50*time.Millisecond, "a session with no members was reported")
	})

	t.Run("reports nothing for a pid that is not a session", func(t *testing.T) {
		t.Parallel()

		// This process did not make a session of its own, so nothing names its pid
		// as one, and a record naming a recycled pid must not kill what holds it.
		assert.False(t, inSession(os.Getpid()))
	})
}

// group starts a session the way the driver does, whose leader exits on SIGTERM
// leaving behind a child that ignores it. It returns the two pids.
func group(t *testing.T) (leader, child int) {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	cmd := osexec.Command("sh", "-c", `trap 'exit 0' TERM; (trap '' TERM; exec sleep 300) & echo $! > "$0"; wait`, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start())

	leader = cmd.Process.Pid

	go func() { _ = cmd.Wait() }()

	t.Cleanup(func() { _ = syscall.Kill(-leader, syscall.SIGKILL) })

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil || len(data) == 0 {
			return false
		}

		child, err = strconv.Atoi(string(data[:len(data)-1]))

		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the leader did not report its child")

	return leader, child
}
