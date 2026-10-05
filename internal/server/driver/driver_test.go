package driver_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestNewWorkload(t *testing.T) {
	t.Parallel()

	t.Run("maps resolved volume mounts", func(t *testing.T) {
		w, err := driver.NewWorkload(row(t, manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
			Volumes: []manifest.VolumeMount{
				{Name: "example-data", From: "/var/lib/takt/volumes/abc", To: "/var/lib/example", ReadOnly: true},
				// Unresolved, so the server has not finished settling the
				// workload. Mounting nothing would be worse than waiting.
				{Name: "pending-data", To: "/var/lib/pending"},
				// A mounted value is written as the workload starts and added by
				// whoever wrote it, so it is not resolved here.
				{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
			},
		}), nil)
		require.NoError(t, err)

		assert.Equal(t, []driver.Volume{
			{Name: "example-data", Host: "/var/lib/takt/volumes/abc", Target: "/var/lib/example", ReadOnly: true},
		}, w.Volumes)
	})

	t.Run("maps path mounts to where they reach", func(t *testing.T) {
		root := tempRoot(t)
		require.NoError(t, os.MkdirAll(filepath.Join(root, "var", "run"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "var", "run", "docker.sock"), nil, 0o600))
		require.NoError(t, os.Symlink(filepath.Join(root, "var", "run"), filepath.Join(root, "run")))

		// The second mount names the socket through a link, and the prefix names it
		// directly. Both sides are resolved, so the driver is handed the real path.
		w, err := driver.NewWorkload(row(t, manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
			Volumes: []manifest.VolumeMount{
				{Path: filepath.Join(root, "media"), To: "/media", ReadOnly: true},
				{Path: filepath.Join(root, "run", "docker.sock"), To: "/var/run/docker.sock"},
			},
		}), []driver.HostPath{{Path: filepath.Join(root, "media")}, {Path: filepath.Join(root, "var", "run", "docker.sock")}})
		require.NoError(t, err)

		// The path as written is what the mount is called, since a path mount has
		// no name of its own, and where it reaches is what the driver binds.
		assert.Equal(t, []driver.Volume{
			{Name: filepath.Join(root, "media"), Host: filepath.Join(root, "media"), Target: "/media", ReadOnly: true},
			{Name: filepath.Join(root, "run", "docker.sock"), Host: filepath.Join(root, "var", "run", "docker.sock"), Target: "/var/run/docker.sock"},
		}, w.Volumes)
	})

	t.Run("refuses a path mount that reaches outside the allowed prefixes", func(t *testing.T) {
		root := tempRoot(t)
		require.NoError(t, os.Mkdir(filepath.Join(root, "media"), 0o755))
		require.NoError(t, os.Symlink(root, filepath.Join(root, "media", "escape")))

		// The link was allowed when the workload was applied, or has been swapped in
		// since. Either way the start is what binds it, so the start is refused.
		_, err := driver.NewWorkload(row(t, manifest.Spec{
			Version:   "v1",
			Name:      "example",
			Container: &manifest.Container{Image: "example/example:latest"},
			Volumes:   []manifest.VolumeMount{{Path: filepath.Join(root, "media", "escape"), To: "/host"}},
		}), []driver.HostPath{{Path: filepath.Join(root, "media")}})
		assert.ErrorIs(t, err, driver.ErrHostPathDenied)
	})
}

func TestResolveHostPath(t *testing.T) {
	t.Parallel()

	root := tempRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "mnt", "media"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "var", "run"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, "var", "run"), filepath.Join(root, "run")))
	require.NoError(t, os.Symlink(root, filepath.Join(root, "mnt", "media", "escape")))
	require.NoError(t, os.Symlink(filepath.Join(root, "mnt", "media"), filepath.Join(root, "mnt", "library")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "var", "run", "pipe"), 0o600))

	// A unix socket path is capped at a length t.TempDir can exceed, so the listener
	// is opened through a short link to the tree.
	short, err := os.MkdirTemp("/tmp", "takt")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(short) })
	require.NoError(t, os.Symlink(filepath.Join(root, "var", "run"), filepath.Join(short, "run")))
	socket, err := net.Listen("unix", filepath.Join(short, "run", "docker.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { socket.Close() })

	tt := []struct {
		Name         string
		Path         string
		Prefixes     []driver.HostPath
		ReadOnly     bool
		Expected     string
		ExpectsError error
	}{
		{
			Name:     "a path under a prefix",
			Path:     filepath.Join(root, "mnt", "media", "films"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "mnt", "media")}},
			Expected: filepath.Join(root, "mnt", "media", "films"),
		},
		{
			Name:     "the prefix itself",
			Path:     filepath.Join(root, "mnt", "media"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "mnt", "media")}},
			Expected: filepath.Join(root, "mnt", "media"),
		},
		{
			Name:     "a root prefix opens everything",
			Path:     filepath.Join(root, "mnt", "media", "escape", "etc"),
			Prefixes: []driver.HostPath{{Path: "/"}},
			Expected: filepath.Join(root, "etc"),
		},
		{
			// The sibling shares the prefix as a string but not as a directory.
			Name:         "a sibling sharing the prefix as text",
			Path:         filepath.Join(root, "mnt", "media-cache"),
			Prefixes:     []driver.HostPath{{Path: filepath.Join(root, "mnt", "media")}},
			ExpectsError: driver.ErrHostPathDenied,
		},
		{
			// A link beneath the allowed directory points at the root of the tree,
			// so what the mount reaches is not what the prefix covers.
			Name:         "a link under the prefix reaching outside it",
			Path:         filepath.Join(root, "mnt", "media", "escape", "var"),
			Prefixes:     []driver.HostPath{{Path: filepath.Join(root, "mnt", "media")}},
			ExpectsError: driver.ErrHostPathDenied,
		},
		{
			// The leaf does not exist, but the link above it does, and it is the
			// link that decides where the mount reaches.
			Name:         "a missing leaf under a link reaching outside",
			Path:         filepath.Join(root, "mnt", "media", "escape", "missing"),
			Prefixes:     []driver.HostPath{{Path: filepath.Join(root, "mnt", "media")}},
			ExpectsError: driver.ErrHostPathDenied,
		},
		{
			// /var/run is a link to /run on most hosts, and either spelling of the
			// socket should be covered by either spelling of the prefix.
			Name:     "a path through a link the prefix names directly",
			Path:     filepath.Join(root, "run", "docker.sock"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "var", "run", "docker.sock")}},
			Expected: filepath.Join(root, "var", "run", "docker.sock"),
		},
		{
			Name:     "a prefix that is itself a link",
			Path:     filepath.Join(root, "mnt", "media", "films"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "mnt", "library")}},
			Expected: filepath.Join(root, "mnt", "media", "films"),
		},
		{
			Name:     "a path that does not exist yet under a prefix",
			Path:     filepath.Join(root, "mnt", "media", "later", "deeper"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "mnt", "media")}},
			Expected: filepath.Join(root, "mnt", "media", "later", "deeper"),
		},
		{
			Name:         "no prefixes",
			Path:         filepath.Join(root, "mnt", "media"),
			ExpectsError: driver.ErrHostPathDenied,
		},
		{
			Name:     "a read-only mount under a read-only prefix",
			Path:     filepath.Join(root, "mnt", "media", "films"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "mnt", "media"), ReadOnly: true}},
			ReadOnly: true,
			Expected: filepath.Join(root, "mnt", "media", "films"),
		},
		{
			Name:         "a writable mount under a read-only prefix",
			Path:         filepath.Join(root, "mnt", "media", "films"),
			Prefixes:     []driver.HostPath{{Path: filepath.Join(root, "mnt", "media"), ReadOnly: true}},
			ExpectsError: driver.ErrHostPathReadOnly,
		},
		{
			// The read-only root would refuse this mount on its own, but the
			// more specific prefix is the one that decides.
			Name: "a writable mount under a writable prefix beneath a read-only root",
			Path: filepath.Join(root, "mnt", "media", "films"),
			Prefixes: []driver.HostPath{
				{Path: "/", ReadOnly: true},
				{Path: filepath.Join(root, "mnt", "media")},
			},
			Expected: filepath.Join(root, "mnt", "media", "films"),
		},
		{
			// Listed the other way round, to show that order does not decide.
			Name: "a writable mount outside the writable prefix beneath a read-only root",
			Path: filepath.Join(root, "var", "run"),
			Prefixes: []driver.HostPath{
				{Path: filepath.Join(root, "mnt", "media")},
				{Path: "/", ReadOnly: true},
			},
			ExpectsError: driver.ErrHostPathReadOnly,
		},
		{
			// The link reaches the read-only prefix, so what it reaches is what is
			// judged, and a writable mount of it is refused.
			Name: "a writable mount through a link into a read-only prefix",
			Path: filepath.Join(root, "mnt", "library", "films"),
			Prefixes: []driver.HostPath{
				{Path: filepath.Join(root, "mnt", "media"), ReadOnly: true},
			},
			ExpectsError: driver.ErrHostPathReadOnly,
		},
		{
			// A read-only mount does not stop a connect on the socket, so the grant
			// the prefix makes would be wider than it says.
			Name:         "a read-only mount of a socket under a read-only prefix",
			Path:         filepath.Join(root, "run", "docker.sock"),
			Prefixes:     []driver.HostPath{{Path: "/", ReadOnly: true}},
			ReadOnly:     true,
			ExpectsError: driver.ErrHostPathNotFile,
		},
		{
			Name:         "a read-only mount of a FIFO under a read-only prefix",
			Path:         filepath.Join(root, "var", "run", "pipe"),
			Prefixes:     []driver.HostPath{{Path: filepath.Join(root, "var"), ReadOnly: true}},
			ReadOnly:     true,
			ExpectsError: driver.ErrHostPathNotFile,
		},
		{
			// The more specific prefix names the socket without the suffix, so the
			// operator opened it on purpose.
			Name: "a socket under a read-only root named by its own prefix",
			Path: filepath.Join(root, "run", "docker.sock"),
			Prefixes: []driver.HostPath{
				{Path: "/", ReadOnly: true},
				{Path: filepath.Join(root, "var", "run", "docker.sock")},
			},
			ReadOnly: true,
			Expected: filepath.Join(root, "var", "run", "docker.sock"),
		},
		{
			Name:     "a read-only mount of a socket under a writable prefix",
			Path:     filepath.Join(root, "run", "docker.sock"),
			Prefixes: []driver.HostPath{{Path: filepath.Join(root, "var", "run")}},
			ReadOnly: true,
			Expected: filepath.Join(root, "var", "run", "docker.sock"),
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			actual, err := driver.ResolveHostPath(tc.Path, tc.ReadOnly, tc.Prefixes)
			if tc.ExpectsError != nil {
				assert.ErrorIs(t, err, tc.ExpectsError)
				assert.Empty(t, actual)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, actual)
		})
	}
}

func TestParseHostPath(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Entry        string
		Expected     driver.HostPath
		ExpectsError bool
	}{
		{
			Name:     "a bare prefix",
			Entry:    "/mnt/media",
			Expected: driver.HostPath{Path: "/mnt/media"},
		},
		{
			Name:     "a read-only prefix",
			Entry:    "/:ro",
			Expected: driver.HostPath{Path: "/", ReadOnly: true},
		},
		{
			// A colon anywhere but the suffix is part of the path.
			Name:     "a colon inside the path",
			Entry:    "/mnt/a:b/c",
			Expected: driver.HostPath{Path: "/mnt/a:b/c"},
		},
		{
			Name:     "a prefix spelt writable",
			Entry:    "/mnt/media:rw",
			Expected: driver.HostPath{Path: "/mnt/media"},
		},
		{
			// Only docker's pair is recognised, so anything else after a colon is
			// a path that happens to end that way.
			Name:     "a suffix that is neither",
			Entry:    "/mnt/media:z",
			Expected: driver.HostPath{Path: "/mnt/media:z"},
		},
		{
			Name:         "a relative prefix",
			Entry:        "media",
			ExpectsError: true,
		},
		{
			Name:         "the suffix alone",
			Entry:        ":ro",
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			actual, err := driver.ParseHostPath(tc.Entry)
			if tc.ExpectsError {
				assert.Error(t, err)
				assert.Zero(t, actual)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, actual)
		})
	}
}

// tempRoot returns a temporary directory with its own links followed, so a
// temporary root that is itself reached through a link does not skew what the
// tests expect a path to resolve to.
func tempRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	return root
}

// row encodes a specification the way the server stores one, so the mapping under
// test reads what a driver would actually be given.
func row(t *testing.T, spec manifest.Spec) database.Workload {
	t.Helper()

	encoded, err := json.Marshal(spec)
	require.NoError(t, err)

	return database.Workload{ID: "cvhs0dq0kqj4c9r8m1a0", Name: spec.Name, Version: 1, Spec: encoded}
}
