package secret_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/secret"
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

func TestStore(t *testing.T) {
	t.Parallel()

	t.Run("creates a key readable only by its owner", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "keys")

		store, err := secret.NewStore(dir)
		require.NoError(t, err)

		id, err := store.Create()
		require.NoError(t, err)

		key, err := store.Read(id)
		require.NoError(t, err)
		assert.Len(t, key, secret.KeyLength)

		info, err := os.Stat(filepath.Join(dir, id+".key"))
		require.NoError(t, err)

		// Anything that can read a key can read every secret sealed under it.
		assert.Zero(t, info.Mode().Perm()&0o077)
	})

	t.Run("returns the same key on a later read", func(t *testing.T) {
		store := newTestStore(t)

		id, err := store.Create()
		require.NoError(t, err)

		first, err := store.Read(id)
		require.NoError(t, err)

		// A server that sealed something has to be able to open it again.
		second, err := store.Read(id)
		require.NoError(t, err)
		assert.Equal(t, first, second)
	})

	// Each key is its own file, so rotating one never writes over the key still in
	// use. That is what makes a rekey recoverable rather than a point of no return.
	t.Run("keeps keys apart from each other", func(t *testing.T) {
		store := newTestStore(t)

		first, err := store.Create()
		require.NoError(t, err)
		second, err := store.Create()
		require.NoError(t, err)

		assert.NotEqual(t, first, second)

		firstKey, err := store.Read(first)
		require.NoError(t, err)
		secondKey, err := store.Read(second)
		require.NoError(t, err)
		assert.NotEqual(t, firstKey, secondKey)

		ids, err := store.List()
		require.NoError(t, err)
		slices.Sort(ids)

		expected := []string{first, second}
		slices.Sort(expected)
		assert.Equal(t, expected, ids)
	})

	t.Run("creates the directory holding the keyring", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "keys")

		_, err := secret.NewStore(dir)
		require.NoError(t, err)

		assert.DirExists(t, dir)
	})

	t.Run("lists nothing in an empty keyring", func(t *testing.T) {
		ids, err := newTestStore(t).List()
		require.NoError(t, err)
		assert.Empty(t, ids)
	})

	// A key the database still points at but the keyring does not hold is the one
	// failure that has to be loud: every secret sealed under it is unreadable.
	t.Run("reports a key that is not there", func(t *testing.T) {
		_, err := newTestStore(t).Read("nothing")
		assert.ErrorIs(t, err, secret.ErrKeyNotFound)
	})

	t.Run("removes a key", func(t *testing.T) {
		store := newTestStore(t)

		id, err := store.Create()
		require.NoError(t, err)
		require.NoError(t, store.Remove(id))

		_, err = store.Read(id)
		assert.ErrorIs(t, err, secret.ErrKeyNotFound)

		// Removing one that is not there is not a failure, so a sweep does not have
		// to race whatever else is removing keys.
		assert.NoError(t, store.Remove(id))
	})

	t.Run("refuses a key of the wrong length", func(t *testing.T) {
		store, id := writeKey(t, 0o600, []byte("too short"))

		// Truncating the key rather than reporting it would silently seal everything
		// under something weaker than what was asked for.
		_, err := store.Read(id)
		assert.ErrorIs(t, err, secret.ErrInvalidKey)
	})

	t.Run("refuses a key others can read", func(t *testing.T) {
		store, id := writeKey(t, 0o644, make([]byte, secret.KeyLength))

		// Whoever else could read it has already had the chance, so starting anyway
		// would report every secret as protected when one of them may not be.
		_, err := store.Read(id)
		assert.ErrorIs(t, err, secret.ErrKeyReadable)
	})

	t.Run("refuses a key others can write", func(t *testing.T) {
		store, id := writeKey(t, 0o622, make([]byte, secret.KeyLength))

		// Replacing a key is enough to make takt seal new values under one somebody
		// else chose, without ever reading the one it had.
		_, err := store.Read(id)
		assert.ErrorIs(t, err, secret.ErrKeyReadable)
	})
}

func newTestStore(t *testing.T) *secret.Store {
	t.Helper()

	store, err := secret.NewStore(filepath.Join(t.TempDir(), "keys"))
	require.NoError(t, err)

	return store
}

// writeKey puts a key file into a keyring with exactly the permissions asked for,
// and returns the store and the key's identifier.
//
// The mode is applied with Chmod rather than left to WriteFile. WriteFile's mode is a
// request the process umask filters, so a test asking for a group-writable file gets
// one only on a host whose umask permits it. That made these tests pass locally under
// umask 002 and fail in CI under umask 022, which is the opposite of what a permission
// test should depend on.
func writeKey(t *testing.T, mode os.FileMode, key []byte) (*secret.Store, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "keys")
	store, err := secret.NewStore(dir)
	require.NoError(t, err)

	const id = "planted"

	path := filepath.Join(dir, id+".key")
	require.NoError(t, os.WriteFile(path, key, mode))
	require.NoError(t, os.Chmod(path, mode))

	// The point of the test is the mode, so a host that would not give us the one we
	// asked for has to say so rather than quietly exercise a different case.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, info.Mode().Perm(), "the test needs a key file with mode %#o", mode)

	return store, id
}

func newTestCipher(t *testing.T) *secret.Cipher {
	t.Helper()

	store := newTestStore(t)

	id, err := store.Create()
	require.NoError(t, err)

	key, err := store.Read(id)
	require.NoError(t, err)

	c, err := secret.New(key)
	require.NoError(t, err)

	return c
}
