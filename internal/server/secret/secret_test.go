package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/secret"
)

func TestCipher_Seal(t *testing.T) {
	t.Parallel()

	t.Run("opens what it sealed", func(t *testing.T) {
		c := newTestCipher(t)

		sealed, err := c.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		opened, err := c.Open("db-password", sealed)
		require.NoError(t, err)
		assert.Equal(t, []byte("hunter2"), opened)
	})

	t.Run("hides the value it sealed", func(t *testing.T) {
		c := newTestCipher(t)

		sealed, err := c.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		assert.NotContains(t, string(sealed), "hunter2")
	})

	t.Run("produces different bytes each time", func(t *testing.T) {
		c := newTestCipher(t)

		first, err := c.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		second, err := c.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		// A per-call nonce is what stops two secrets holding the same value from
		// being recognisable as equal in the database.
		assert.NotEqual(t, first, second)
	})

	t.Run("seals an empty value", func(t *testing.T) {
		c := newTestCipher(t)

		sealed, err := c.Seal("db-password", nil)
		require.NoError(t, err)

		opened, err := c.Open("db-password", sealed)
		require.NoError(t, err)
		assert.Empty(t, opened)
	})
}

func TestCipher_Open(t *testing.T) {
	t.Parallel()

	t.Run("refuses a value sealed under another name", func(t *testing.T) {
		c := newTestCipher(t)

		sealed, err := c.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		// The name is authenticated, so a ciphertext moved to another row fails to
		// open rather than decrypting as whichever secret now holds it.
		_, err = c.Open("api-token", sealed)
		assert.ErrorIs(t, err, secret.ErrInvalidCiphertext)
	})

	t.Run("refuses a tampered value", func(t *testing.T) {
		c := newTestCipher(t)

		sealed, err := c.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		sealed[len(sealed)-1] ^= 0xff

		_, err = c.Open("db-password", sealed)
		assert.ErrorIs(t, err, secret.ErrInvalidCiphertext)
	})

	t.Run("refuses a value sealed under another key", func(t *testing.T) {
		sealed, err := newTestCipher(t).Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		_, err = newTestCipher(t).Open("db-password", sealed)
		assert.ErrorIs(t, err, secret.ErrInvalidCiphertext)
	})

	t.Run("refuses a value shorter than a nonce", func(t *testing.T) {
		_, err := newTestCipher(t).Open("db-password", []byte("short"))
		assert.ErrorIs(t, err, secret.ErrInvalidCiphertext)
	})
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("refuses a key of the wrong length", func(t *testing.T) {
		_, err := secret.New(make([]byte, 16))
		assert.ErrorIs(t, err, secret.ErrInvalidKey)
	})
}

func TestLoadKey(t *testing.T) {
	t.Parallel()

	t.Run("generates a key that is readable only by its owner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret.key")

		key, err := secret.LoadKey(path)
		require.NoError(t, err)
		assert.Len(t, key, secret.KeyLength)

		info, err := os.Stat(path)
		require.NoError(t, err)

		// Anything that can read the key can read every secret orca holds.
		assert.Zero(t, info.Mode().Perm()&0o077)
	})

	t.Run("returns the same key on a later start", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret.key")

		first, err := secret.LoadKey(path)
		require.NoError(t, err)

		// A server that sealed something has to be able to open it again.
		second, err := secret.LoadKey(path)
		require.NoError(t, err)
		assert.Equal(t, first, second)
	})

	t.Run("creates the directory holding it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "secret.key")

		_, err := secret.LoadKey(path)
		require.NoError(t, err)

		assert.FileExists(t, path)
	})

	t.Run("refuses a key file of the wrong length", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret.key")
		require.NoError(t, os.WriteFile(path, []byte("too short"), 0o600))

		// Truncating the key rather than reporting it would silently seal everything
		// under something weaker than what was asked for.
		_, err := secret.LoadKey(path)
		assert.ErrorIs(t, err, secret.ErrInvalidKey)
	})
}

func newTestCipher(t *testing.T) *secret.Cipher {
	t.Helper()

	key, err := secret.LoadKey(filepath.Join(t.TempDir(), "secret.key"))
	require.NoError(t, err)

	c, err := secret.New(key)
	require.NoError(t, err)

	return c
}
