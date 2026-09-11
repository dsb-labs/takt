package mount_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/mount"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func TestMounter_Deliver(t *testing.T) {
	t.Parallel()

	t.Run("writes a file for every value the workload mounts", func(t *testing.T) {
		secrets, variables := NewMockValueStore(t), NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Once()
		variables.EXPECT().Value(mock.Anything, "app-config").Return(`{"level":"debug"}`, nil).Once()

		svc, root := newMounter(t, secrets, variables)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem", ReadOnly: true},
			manifest.VolumeMount{Var: "app-config", To: "/etc/app/config.json"},
		))
		require.NoError(t, err)
		require.Len(t, mounts, 2)

		// The driver is handed a path and a target, which is exactly what it is handed
		// for a volume: a mounted value needs nothing new of either runtime. What the
		// mount says about being read-only travels with it, so the bind enforces what
		// the file's own mode only suggests.
		dir := filepath.Join(root, "mounts", "files", testVolumeID, "1")
		assert.Equal(t, filepath.Join(dir, "secret-tls-cert"), mounts[0].Host)
		assert.Equal(t, "/etc/tls/cert.pem", mounts[0].Target)
		assert.True(t, mounts[0].ReadOnly)
		assert.Equal(t, filepath.Join(dir, "var-app-config"), mounts[1].Host)
		assert.Equal(t, "/etc/app/config.json", mounts[1].Target)
		assert.False(t, mounts[1].ReadOnly)

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

		svc, root := newMounter(t, secrets, nil)

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

		svc, _ := newMounter(t, secrets, variables)

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

		svc, _ := newMounter(t, secrets, nil)
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

	t.Run("mints and writes a mounted token", func(t *testing.T) {
		tokens := NewMockTokens(t)

		// A signal is what makes rotation deliverable, so only a signalled mount
		// asks for a token that expires.
		tokens.EXPECT().MintWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_rotating", nil).Once()
		tokens.EXPECT().MintWorkloadToken(mock.Anything, "deploy-runner", testVolumeID, 1, false).
			Return("takt_c_standing", nil).Once()

		svc, _ := newTokenMounter(t, tokens)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Token: "prometheus", To: "/etc/prometheus/takt-token", Signal: manifest.SignalHUP},
			manifest.VolumeMount{Token: "deploy-runner", To: "/var/run/takt/token"},
		))
		require.NoError(t, err)
		require.Len(t, mounts, 2)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_rotating", string(contents))

		contents, err = os.ReadFile(mounts[1].Host)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_standing", string(contents))
	})

	t.Run("keeps the token an earlier delivery minted", func(t *testing.T) {
		// Delivery runs as every instance starts. Minting again would revoke the
		// credential the first instance already read, so a second start keeps
		// the file, and the refresh only answers whether the token behind it
		// still lives.
		tokens := NewMockTokens(t)
		tokens.EXPECT().MintWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_first", nil).Once()
		tokens.EXPECT().RefreshWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("", false, nil).Once()

		svc, _ := newTokenMounter(t, tokens)

		spec := mountSpec(manifest.VolumeMount{Token: "prometheus", To: "/etc/prometheus/takt-token", Signal: manifest.SignalHUP})

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, mounts, 1)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_first", string(contents))
	})

	t.Run("rewrites a token the refresh re-minted", func(t *testing.T) {
		// A token revoked by hand is gone from the database while its file
		// remains, and the next delivery brings it back: the manifest asks for
		// a state, not an action.
		tokens := NewMockTokens(t)
		tokens.EXPECT().MintWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_first", nil).Once()
		tokens.EXPECT().RefreshWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_second", true, nil).Once()

		svc, _ := newTokenMounter(t, tokens)

		spec := mountSpec(manifest.VolumeMount{Token: "prometheus", To: "/etc/prometheus/takt-token", Signal: manifest.SignalHUP})

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_second", string(contents))
	})

	t.Run("refuses a token mount on a server that mints none", func(t *testing.T) {
		svc, _ := newMounter(t, nil, nil)

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Token: "prometheus", To: "/etc/prometheus/takt-token"},
		))
		assert.ErrorIs(t, err, mount.ErrInvalidMount)
	})

	t.Run("ignores a mounted volume", func(t *testing.T) {
		svc, root := newMounter(t, nil, nil)

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

		svc, _ := newMounter(t, secrets, nil)

		// Writing an empty file would hand the workload a value takt does not hold,
		// which it would then use.
		_, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "nope", To: "/etc/tls/cert.pem"},
		))
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})

	t.Run("refuses a value on a server holding no store of that kind", func(t *testing.T) {
		svc, _ := newMounter(t, nil, nil)

		_, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
		))
		assert.ErrorIs(t, err, database.ErrSecretNotFound)
	})

	t.Run("refuses an identifier it cannot use as a directory", func(t *testing.T) {
		svc, _ := newMounter(t, NewMockValueStore(t), nil)

		// A service that removes directories should not build a path from a value it has
		// not looked at.
		for _, id := range []string{"", ".", "..", "../escape", "with/separator", "UPPERCASE"} {
			_, err := svc.Deliver(t.Context(), id, 1, mountSpec(
				manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
			))
			assert.ErrorIs(t, err, mount.ErrInvalidMount, "accepted the identifier %q", id)
		}
	})
}

func TestMounter_Refresh(t *testing.T) {
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

		svc, _ := newMounter(t, secrets, nil)
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

		svc, _ := newMounter(t, secrets, nil)
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

		svc, _ := newMounter(t, secrets, nil)
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

	t.Run("rewrites a rotated token and reports the signal", func(t *testing.T) {
		tokens := NewMockTokens(t)
		tokens.EXPECT().MintWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_first", nil).Once()
		tokens.EXPECT().RefreshWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_rotated", true, nil).Once()

		svc, _ := newTokenMounter(t, tokens)

		spec := mountSpec(manifest.VolumeMount{Token: "prometheus", To: "/etc/prometheus/takt-token", Signal: manifest.SignalHUP})

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, spec)
		require.NoError(t, err)
		require.Len(t, refreshed, 1)
		assert.Equal(t, manifest.Reference{Kind: manifest.KindToken, Name: "prometheus"}, refreshed[0].Reference)
		assert.Equal(t, manifest.SignalHUP, refreshed[0].Signal)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_rotated", string(contents))
	})

	t.Run("reports nothing while the token keeps its life", func(t *testing.T) {
		tokens := NewMockTokens(t)
		tokens.EXPECT().MintWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("takt_c_first", nil).Once()
		tokens.EXPECT().RefreshWorkloadToken(mock.Anything, "prometheus", testVolumeID, 1, true).
			Return("", false, nil).Once()

		svc, _ := newTokenMounter(t, tokens)

		spec := mountSpec(manifest.VolumeMount{Token: "prometheus", To: "/etc/prometheus/takt-token", Signal: manifest.SignalHUP})

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, spec)
		require.NoError(t, err)

		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, spec)
		require.NoError(t, err)
		assert.Empty(t, refreshed)

		contents, err := os.ReadFile(mounts[0].Host)
		require.NoError(t, err)
		assert.Equal(t, "takt_c_first", string(contents))
	})

	t.Run("ignores a mount that asked to be replaced", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("first", nil).Once()

		svc, _ := newMounter(t, secrets, nil)
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
		svc, _ := newMounter(t, NewMockValueStore(t), nil)

		// The workload has not started yet, so there is no file of ours to rewrite and
		// nothing to compare against. Delivery writes both.
		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, signalled())
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})

	t.Run("reports nothing for a workload that mounts no value", func(t *testing.T) {
		svc, _ := newMounter(t, nil, nil)

		refreshed, err := svc.Refresh(t.Context(), "example", testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Name: "example-data", To: "/var/lib/example"},
		))
		require.NoError(t, err)
		assert.Empty(t, refreshed)
	})
}

func TestMounter_Reclaim(t *testing.T) {
	t.Parallel()

	// The leak this exists to stop. Every rotation of a mounted secret left the
	// previous version's plaintext on the disk for the life of the workload.
	t.Run("removes the superseded version's plaintext", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("the first certificate", nil).Once()
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("the second certificate", nil).Once()

		svc, _ := newMounter(t, secrets, nil)

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

		svc, root := newMounter(t, secrets, nil)

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

		svc, _ := newMounter(t, secrets, nil)

		mounts, err := svc.Deliver(t.Context(), testVolumeID, 1, mountSpec(
			manifest.VolumeMount{Secret: "tls-cert", To: "/etc/tls/cert.pem"},
		))
		require.NoError(t, err)

		require.NoError(t, svc.Reclaim(testVolumeID, 1))
		assert.FileExists(t, mounts[0].Host)
	})

	t.Run("accepts a workload it never wrote for", func(t *testing.T) {
		svc, _ := newMounter(t, nil, nil)

		// A workload that mounts nothing has no versions to sweep, which is not a
		// failure: the reconciler calls this after every start.
		assert.NoError(t, svc.Reclaim(testVolumeID, 1))
	})

	t.Run("refuses an identifier it cannot use as a directory", func(t *testing.T) {
		svc, _ := newMounter(t, nil, nil)

		assert.ErrorIs(t, svc.Reclaim("../escape", 1), mount.ErrInvalidMount)
	})
}

func TestMounter_Forget(t *testing.T) {
	t.Parallel()

	t.Run("removes what it wrote for the workload", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Once()

		svc, _ := newMounter(t, secrets, nil)

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
		svc, _ := newMounter(t, nil, nil)

		// The workload mounted nothing, so there is nothing to remove.
		assert.NoError(t, svc.Forget(testVolumeID))
	})

	t.Run("refuses an identifier it cannot use as a directory", func(t *testing.T) {
		svc, _ := newMounter(t, nil, nil)

		for _, id := range []string{"", "..", "../escape", "with/separator"} {
			assert.ErrorIs(t, svc.Forget(id), mount.ErrInvalidMount, "accepted the identifier %q", id)
		}
	})
}

func TestMounter_Prune(t *testing.T) {
	t.Parallel()

	t.Run("removes what it wrote for workloads that no longer exist", func(t *testing.T) {
		secrets := NewMockValueStore(t)
		secrets.EXPECT().Value(mock.Anything, "tls-cert").Return("a certificate", nil).Twice()

		svc, _ := newMounter(t, secrets, nil)
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
		svc, _ := newMounter(t, nil, nil)

		assert.NoError(t, svc.Prune([]string{testVolumeID}))
	})
}

// mountSpec returns a container specification mounting the given mounts, which is all
// the mounter reads of one.
func mountSpec(mounts ...manifest.VolumeMount) manifest.Spec {
	return manifest.Spec{
		Version:   "v1",
		Name:      "example",
		Container: &manifest.Container{Image: "example/example:latest"},
		Volumes:   mounts,
	}
}

// newMounter returns a mounter alongside the data directory it writes
// mounted values under. Either store may be nil, which is a server holding nothing of
// that kind.
func newMounter(t *testing.T, secrets, variables mount.ValueStore) (*mount.Mounter, string) {
	t.Helper()

	root := t.TempDir()

	config := mount.Config{
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

	return mount.New(config), root
}

// newTokenMounter returns a mounter that mints through the given mock and
// holds no value stores, alongside its data directory.
func newTokenMounter(t *testing.T, tokens mount.Tokens) (*mount.Mounter, string) {
	t.Helper()

	root := t.TempDir()

	return mount.New(mount.Config{
		Logger:    newTestLogger(t),
		Tokens:    tokens,
		Directory: root,
	}), root
}

// The identifiers takt assigns are xid values: twenty lowercase alphanumeric
// characters.
const (
	testVolumeID  = "cvhs0dq0kqj4c9r8m1a0"
	otherVolumeID = "cvhs0dq0kqj4c9r8m1a1"
)

func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}
