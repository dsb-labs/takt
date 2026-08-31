package service_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/service"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestMountService_Deliver(t *testing.T) {
	t.Parallel()

	t.Run("writes a file for every value the workload mounts", func(t *testing.T) {
		secrets, variables := NewMockValueStore(t), NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Once()
		variables.EXPECT().Value(mock.Anything, "app-config").Return(`{"level":"debug"}`, nil).Once()

		svc, root := newMountService(t, secrets, variables)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
			manifest.VolumeMount{Var: "app-config", To: "/etc/app/config.json"},
		))
		require.NoError(t, err)
		require.Len(t, mounts, 2)

		// The driver is handed a path and a target, which is exactly what it is handed
		// for a volume: a mounted value needs nothing new of either runtime.
		dir := filepath.Join(root, "mounts", "files", testVolumeID, "1")
		assert.Equal(t, filepath.Join(dir, "secret-tls-cert"), mounts[0].Host)
		assert.Equal(t, "/etc/tls/cert.pem", mounts[0].Target)
		assert.Equal(t, filepath.Join(dir, "var-app-config"), mounts[1].Host)
		assert.Equal(t, "/etc/app/config.json", mounts[1].Target)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "a certificate", string(contents))

		contents, err = os.ReadFile(mounts[1].Host)
		require.NoError(t, err)
		assert.Equal(t, `{"level":"debug"}`, string(contents))
	})

	t.Run("writes a readable file inside a private directory", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Once()

		svc, root := newMountService(t, secrets, nil)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
		))
		require.NoError(t, err)
		require.Len(t, mounts, 1)

		// A container runs as a user of its own, so a file only the server's user could
		// read would be unreadable by the workload that mounted it.
		info, err := os.Stat(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o444), info.Mode().Perm())

		// What keeps it private is the directory above, which nothing else on the host
		// may enter.
		info, err = os.Stat(filepath.Join(root, "mounts", "files", testVolumeID, "1"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	})

	t.Run("keeps a secret and a variable sharing a name apart", func(t *testing.T) {
		secrets, variables := NewMockValueStore(t), NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "shared").Return("the secret", nil).Once()
		variables.EXPECT().Value(mock.Anything, "shared").Return("the variable", nil).Once()

		svc, _ := newMountService(t, secrets, variables)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "shared", To: "/etc/secret"},
			manifest.VolumeMount{Var: "shared", To: "/etc/variable"},
		))
		require.NoError(t, err)
		require.Len(t, mounts, 2)

		// The kind is part of the file's name, because the two are different values.
		assert.NotEqual(t, mounts[0].Host, mounts[1].Host)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "the secret", string(contents))

		contents, err = os.ReadFile(mounts[1].Host)
		require.NoError(t, err)
		assert.Equal(t, "the variable", string(contents))
	})

	t.Run("keeps the versions of a workload apart", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("first", nil).Once()
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("second", nil).Once()

		svc, _ := newMountService(t, secrets, nil)
		spec := mountSpec(manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"})

		first, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		second, err := svc.Deliver(t.Context(), testVolumeID, 2, spec)
		require.NoError(t, err)

		// A replacement must not overwrite the file the instance it replaces is still
		// reading, which is why the directory is keyed by version.
		require.Len(t, first, 1)
		require.Len(t, second, 1)
		assert.NotEqual(t, first[0].Host, second[0].Host)

		contents, err := os.ReadFile(first[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "first", string(contents))
	})

	t.Run("ignores a mounted volume", func(t *testing.T) {
		svc, root := newMountService(t, nil, nil)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Name: "example-data", To: "/var/lib/example", From: "/somewhere"},
		))
		require.NoError(t, err)
		assert.Empty(t, mounts)

		// A workload mounting no value writes nothing and creates no directories.
		_, err = os.Stat(filepath.Join(root, "mounts"))
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("refuses a value the server does not hold", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "nope").
			Return("", database.ErrSecretNotFound).Once()

		svc, _ := newMountService(t, secrets, nil)

		// Writing an empty file would hand the workload a value orca does not hold,
		// which it would then use.
		_, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "nope", To: "/etc/tls/cert.pem"},
		))
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})

	t.Run("refuses a value on a server holding no store of that kind", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
		))
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})

	t.Run("refuses an identifier it cannot use as a directory", func(t *testing.T) {
		svc, _ := newMountService(t, NewMockValueStore(t), nil)

		// A service that removes directories should not build a path from a value it has
		// not looked at.
		for _, id := range []string{"", ".", "..", "../escape", "with/separator", "UPPERCASE"} {
			_, err := svc.Deliver(t.Context(), id, 1, mountSpec(
				manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
			))
			assert.ErrorIs(t, err, service.ErrInvalidMount, "accepted the identifier %q", id)
		}
	})
}

func TestMountService_Refresh(t *testing.T) {
	t.Parallel()

	signalled := func() manifest.Spec {
		return mountSpec(manifest.VolumeMount{
			Secret: "tls-cert",
			To:     "/etc/tls/cert.pem",
			Signal: manifest.SignalHUP,
		})
	}

	t.Run("rewrites a changed value and reports the signal", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("first", nil).Once()
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("second", nil).Once()

		svc, _ := newMountService(t, secrets, nil)
		spec := signalled()

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, mounts, 1)

		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, refreshed, 1)

		assert.Equal(t, manifest.SignalHUP, refreshed[0].Signal)
		assert.Equal(t, manifest.KindSecret, refreshed[0].Reference.Kind)
		assert.Equal(t, "tls-cert", refreshed[0].Reference.Name)

		// The same file rather than a replacement, because a bind mount follows the
		// inode: a swapped file would leave the container reading the old contents
		// forever.
		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "second", string(contents))
	})

	t.Run("reports nothing when the value is unchanged", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("unchanged", nil).Twice()

		svc, _ := newMountService(t, secrets, nil)
		spec := signalled()

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		// Nothing to tell the workload about, so it is not disturbed.
		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, spec)
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})

	t.Run("keeps the file readable after a rewrite", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("first", nil).Once()
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("second", nil).Once()

		svc, _ := newMountService(t, secrets, nil)
		spec := signalled()

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, mounts, 1)

		_, err = svc.Refresh(t.Context(), "example", testVolumeID, 1, spec)
		require.NoError(t, err)

		// A rewrite that narrowed the mode would leave the workload unable to read what
		// it was just told to reload.
		info, err := os.Stat(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o444), info.Mode().Perm())
	})

	t.Run("ignores a mount that asked to be replaced", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("first", nil).Once()

		svc, _ := newMountService(t, secrets, nil)
		spec := mountSpec(manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"})

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, mounts, 1)

		// Such a mount is delivered by replacing the instance, which the specification's
		// hash arranges. Rewriting the file underneath the running workload would change
		// what it read without telling it.
		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, spec)
		require.NoError(t, err)
		assert.Empty(t, refreshed)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "first", string(contents))
	})

	t.Run("reports nothing for a version that was never delivered", func(t *testing.T) {
		svc, _ := newMountService(t, NewMockValueStore(t), nil)

		// The workload has not started yet, so there is no file of ours to rewrite and
		// nothing to compare against. Delivery writes both.
		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, signalled())
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})

	t.Run("reports nothing for a workload that mounts no value", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Name: "example-data", To: "/var/lib/example"},
		))
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})
}

func TestMountService_Reclaim(t *testing.T) {
	t.Parallel()

	// The leak this exists to stop. Every rotation of a mounted secret left the
	// previous version's plaintext on the disk for the life of the workload.
	t.Run("removes the superseded version's plaintext", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("the first certificate", nil).Once()
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("the second certificate", nil).Once()

		svc, _ := newMountService(t, secrets, nil)

		spec := mountSpec(manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"})

		first, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, first, 1)

		second, err := svc.Deliver(t.Context(), testVolumeID, 2, spec)
		require.NoError(t, err)
		require.Len(t, second, 1)

		// Both are on the disk before the sweep, or this would pass without doing
		// anything.
		require.FileExists(t, first[0].Host)
		require.FileExists(t, second[0].Host)

		require.NoError(t, svc.Reclaim(testVolumeID, 2))

		_, err = os.Stat(first[0].Host)
		assert.True(t, os.IsNotExist(err), "the superseded value is still on the disk")

		// The version the workload is running keeps its files, which it is still
		// reading.
		assert.FileExists(t, second[0].Host)
	})

	// The two trees are shaped differently — a directory per version of the files, a
	// file per version of the records — and the first version of this swept only the
	// directories, so the records kept accumulating unnoticed.
	t.Run("removes the superseded version's record", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Twice()

		svc, root := newMountService(t, secrets, nil)

		spec := mountSpec(manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"})

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		_, err = svc.Deliver(t.Context(), testVolumeID, 2, spec)
		require.NoError(t, err)

		state := filepath.Join(root, "mounts", "state", testVolumeID)

		before, err := os.ReadDir(state)
		require.NoError(t, err)
		require.Len(t, before, 2, "the test needs a record per version to sweep")

		require.NoError(t, svc.Reclaim(testVolumeID, 2))

		after, err := os.ReadDir(state)
		require.NoError(t, err)
		require.Len(t, after, 1)
		assert.Equal(t, "2.json", after[0].Name())
	})

	t.Run("keeps a workload that holds only the version named", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Once()

		svc, _ := newMountService(t, secrets, nil)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
		))
		require.NoError(t, err)

		require.NoError(t, svc.Reclaim(testVolumeID, 1))
		assert.FileExists(t, mounts[0].Host)
	})

	t.Run("accepts a workload it never wrote for", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		// A workload that mounts nothing has no versions to sweep, which is not a
		// failure: the reconciler calls this after every start.
		assert.NoError(t, svc.Reclaim(testVolumeID, 1))
	})

	t.Run("refuses an identifier it cannot use as a directory", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		assert.ErrorIs(t, svc.Reclaim("../escape", 1), service.ErrInvalidMount)
	})
}

func TestMountService_Forget(t *testing.T) {
	t.Parallel()

	t.Run("removes what it wrote for the workload", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Once()

		svc, _ := newMountService(t, secrets, nil)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
		))
		require.NoError(t, err)
		require.Len(t, mounts, 1)

		require.NoError(t, svc.Forget(testVolumeID))

		// This is what takes a mounted secret's plaintext off the disk.
		_, err = os.Stat(mounts[0].Host)
		assert.True(t, os.IsNotExist(err), "the mounted value outlived the workload")
	})

	t.Run("accepts a workload it never wrote for", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		// The workload mounted nothing, so there is nothing to remove.
		assert.NoError(t, svc.Forget(testVolumeID))
	})

	t.Run("refuses an identifier it cannot use as a directory", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		for _, id := range []string{"", "..", "../escape", "with/separator"} {
			assert.ErrorIs(t, svc.Forget(id), service.ErrInvalidMount, "accepted the identifier %q", id)
		}
	})
}

func TestMountService_Prune(t *testing.T) {
	t.Parallel()

	t.Run("removes what it wrote for workloads that no longer exist", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Twice()

		svc, _ := newMountService(t, secrets, nil)
		spec := mountSpec(manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"})

		kept, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, kept, 1)

		gone, err := svc.Deliver(t.Context(), otherVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, gone, 1)

		require.NoError(t, svc.Prune([]string{testVolumeID}))

		// A teardown removes these itself. This is for the server that stopped between
		// stopping the work and removing the row, which would otherwise leave a secret's
		// plaintext waiting for something to notice.
		_, err = os.Stat(gone[0].Host)
		assert.True(t, os.IsNotExist(err), "the mounted value of a departed workload was kept")

		_, err = os.Stat(kept[0].Host)
		assert.NoError(t, err, "the mounted value of a live workload was removed")
	})

	t.Run("accepts a host where nothing has been delivered", func(t *testing.T) {
		svc, _ := newMountService(t, nil, nil)

		assert.NoError(t, svc.Prune([]string{testVolumeID}))
	})
}

// mountSpec returns a container specification mounting the given mounts, which is all
// the mount service reads of one.
func mountSpec(mounts ...manifest.VolumeMount) manifest.Spec {
	spec := containerSpec("example", "example/example:latest")
	spec.Volumes = mounts

	return spec
}

// newMountService returns a mount service alongside the data directory it writes
// mounted values under. Either store may be nil, which is a server holding nothing of
// that kind.
func newMountService(t *testing.T, secrets, variables service.ValueStore) (*service.MountService, string) {
	t.Helper()

	root := t.TempDir()

	config := service.MountServiceConfig{
		Logger:    newTestLogger(t),
		Directory: root,
	}

	// Assigned only when given, so that a nil mock is a server with no store rather
	// than a typed nil that answers calls.
	if secrets != nil {
		config.Secrets = secrets
	}
	if variables != nil {
		config.Variables = variables
	}

	return service.NewMountService(config), root
}
