//go:build linux

package exec_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/driver"
)

func TestDriver_Exec(t *testing.T) {
	t.Parallel()

	t.Run("reports the status and splits the output by stream", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		var stdout, stderr bytes.Buffer

		status, err := d.Exec(t.Context(), w, command("echo to-stdout; echo to-stderr >&2; exit 3", nil, &stdout, &stderr))
		require.NoError(t, err)

		assert.Equal(t, 3, status)
		assert.Equal(t, "to-stdout\n", stdout.String())
		assert.Equal(t, "to-stderr\n", stderr.String())
	})

	t.Run("copies the input to the command", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		var stdout bytes.Buffer

		// cat ends when its input does, so this also proves the end of the reader
		// reaches the command as end of file.
		status, err := d.Exec(t.Context(), w, command("cat", strings.NewReader("typed\n"), &stdout, io.Discard))
		require.NoError(t, err)

		assert.Zero(t, status)
		assert.Equal(t, "typed\n", stdout.String())
	})

	t.Run("gives a command without input an input that has ended", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		// A cat that waited on a terminal would hang here until the deadline.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		status, err := d.Exec(ctx, w, command("cat", nil, io.Discard, io.Discard))
		require.NoError(t, err)
		assert.Zero(t, status)
	})

	t.Run("kills a command that outlives the context", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()

		started := time.Now()

		// The sleep is a child of the shell, so the kill has to reach the group
		// rather than the shell alone for this to return promptly.
		_, err := d.Exec(ctx, w, command("sleep 30", nil, io.Discard, io.Discard))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 5*time.Second)
	})

	t.Run("runs in the instance's working directory", func(t *testing.T) {
		t.Parallel()

		d, root := newDriver(t)
		w := startRunning(t, d)

		require.NoError(t, os.WriteFile(filepath.Join(root, "workloads", idOf(t), "0", "1", "cwd", "ready"), nil, 0o600))

		status, err := d.Exec(t.Context(), w, command("test -f ready", nil, io.Discard, io.Discard))
		require.NoError(t, err)
		assert.Zero(t, status)
	})

	t.Run("is confined as the instance is", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)
		w := startRunning(t, d)

		secret := filepath.Join(t.TempDir(), "secret.key")
		require.NoError(t, os.WriteFile(secret, []byte("SEALED"), 0o600))

		var stdout, stderr bytes.Buffer

		// Readable by whoever runs the tests, so an unconfined command would print it.
		status, err := d.Exec(t.Context(), w, command("cat "+secret, nil, &stdout, &stderr))
		require.NoError(t, err)

		assert.NotZero(t, status)
		assert.NotContains(t, stdout.String(), "SEALED", "a confined command read a file it was never granted")
		assert.Contains(t, stderr.String(), "Permission denied")
	})

	t.Run("refuses an instance that is not running", func(t *testing.T) {
		t.Parallel()

		d, _ := newDriver(t)

		_, err := d.Exec(t.Context(), workload(t, "example", 1, "hash", "exit 0"), command("exit 0", nil, io.Discard, io.Discard))
		assert.ErrorIs(t, err, driver.ErrNoRunningInstance)
	})
}

// command describes an operator's command running the given shell script with an
// empty environment.
func command(script string, stdin io.Reader, stdout, stderr io.Writer) driver.Exec {
	return driver.Exec{
		Command: []string{"sh", "-c", script},
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		Env:     func(context.Context) (map[string]string, error) { return nil, nil },
	}
}
