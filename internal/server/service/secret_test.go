package service_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/secret"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestSecretService_Set(t *testing.T) {
	t.Parallel()

	t.Run("stores a secret that does not exist", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{}, database.ErrSecretNotFound).Once()
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, value []byte, revision, _ string, _ map[string]string) (database.Secret, error) {
				// What reaches the database is the sealed value, never the plaintext.
				assert.NotContains(t, string(value), "hunter2")

				return database.Secret{Name: name, Value: value, Revision: revision}, nil
			}).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		stored, created, err := newTestSecretService(t, secrets, nil).
			Set(t.Context(), "db-password", []byte("hunter2"), nil)
		require.NoError(t, err)
		assert.True(t, created)
		assert.Equal(t, "db-password", stored.Name)
		assert.NotEmpty(t, stored.Revision)
	})

	t.Run("moves the revision when the value changes", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)
		cipher := newTestCipher(t)

		sealed, err := cipher.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: sealed, Revision: "rev-one"}, nil).Once()

		var revision string
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, value []byte, rev, _ string, _ map[string]string) (database.Secret, error) {
				revision = rev

				return database.Secret{Name: name, Value: value, Revision: rev}, nil
			}).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		stored, created, err := newTestSecretService(t, secrets, cipher).
			Set(t.Context(), "db-password", []byte("hunter3"), nil)
		require.NoError(t, err)
		assert.False(t, created)
		assert.NotEqual(t, "rev-one", revision)
		assert.Equal(t, revision, stored.Revision)
	})

	t.Run("leaves the revision alone when the value is unchanged", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)
		cipher := newTestCipher(t)

		sealed, err := cipher.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: sealed, Revision: "rev-one"}, nil).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		// Setting a secret to what it already holds writes nothing, so a tool that
		// sets every secret on every run does not restart the fleet each time. The
		// mock asserts no Upsert, since it was never told to expect one.
		stored, created, err := newTestSecretService(t, secrets, cipher).
			Set(t.Context(), "db-password", []byte("hunter2"), nil)
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, "rev-one", stored.Revision)
	})

	t.Run("writes labels without moving the revision", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)
		cipher := newTestCipher(t)

		sealed, err := cipher.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		existing := database.Secret{
			Name:     "db-password",
			Value:    sealed,
			Revision: "rev-one",
			KeyID:    "key-one",
			Labels:   map[string]string{"app": "web"},
		}

		secrets.EXPECT().Get(mock.Anything, "db-password").Return(existing, nil).Once()

		// The revision it already holds goes back in. It is what moves the
		// specification hash of every workload reading the secret, and labelling one
		// is bookkeeping — replacing instances across the node for that would be
		// absurd.
		secrets.EXPECT().
			Upsert(mock.Anything, "db-password", sealed, "rev-one", "key-one", map[string]string{"app": "api"}).
			Return(database.Secret{
				Name:     "db-password",
				Value:    sealed,
				Revision: "rev-one",
				KeyID:    "key-one",
				Labels:   map[string]string{"app": "api"},
			}, nil).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		// Nothing is redeployed, which the mock asserts by never being told to
		// expect a rehash.
		stored, created, err := newTestSecretService(t, secrets, cipher).
			Set(t.Context(), "db-password", []byte("hunter2"), map[string]string{"app": "api"})
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, "rev-one", stored.Revision)
		assert.Equal(t, map[string]string{"app": "api"}, stored.Labels)
	})

	t.Run("refuses a label orca reserves for itself", func(t *testing.T) {
		// The rules are the workload's rules. The repository is never reached, which
		// the mock asserts by expecting nothing.
		_, _, err := newTestSecretService(t, NewMockSecretRepository(t), nil).
			Set(t.Context(), "db-password", []byte("hunter2"), map[string]string{"orca.workload": "sneaky"})
		assert.ErrorIs(t, err, service.ErrInvalidSecret)
	})

	t.Run("redeploys the workloads reading it", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{}, database.ErrSecretNotFound).Once()
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(database.Secret{Name: "db-password", Revision: "rev-one"}, nil).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").
			Return([]string{"one", "two"}, nil).Twice()

		var rehashed []string
		svc := service.NewSecretService(service.SecretServiceConfig{
			Logger:  newTestLogger(t),
			Secrets: secrets,
			Cipher:  newTestCipher(t),
			Rehash: func(_ context.Context, workload string) error {
				rehashed = append(rehashed, workload)

				return nil
			},
		})

		_, _, err := svc.Set(t.Context(), "db-password", []byte("hunter2"), nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"one", "two"}, rehashed)
	})

	t.Run("refuses a name orca would not accept", func(t *testing.T) {
		_, _, err := newTestSecretService(t, NewMockSecretRepository(t), nil).
			Set(t.Context(), "DB_PASSWORD", []byte("hunter2"), nil)
		assert.ErrorIs(t, err, service.ErrInvalidSecret)
	})

	t.Run("re-seals a value it cannot open", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: []byte("sealed under a key that is gone")}, nil).Once()
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(database.Secret{Name: "db-password", Revision: "rev-two"}, nil).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		// A key rotated out from under the database leaves a value nothing can read.
		// Re-sealing under the current key is more useful than refusing to move.
		_, _, err := newTestSecretService(t, secrets, nil).Set(t.Context(), "db-password", []byte("hunter2"), nil)
		require.NoError(t, err)
	})
}

func TestSecretService_List_Queries(t *testing.T) {
	t.Parallel()

	t.Run("passes parsed queries to the repository", func(t *testing.T) {
		t.Parallel()

		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().List(mock.Anything, []database.Query{{Path: "$.labels.app", Value: "web"}}).
			Return([]database.Secret{{Name: "db-password", Revision: "rev-one"}}, nil).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		got, err := newTestSecretService(t, secrets, nil).List(t.Context(), "$.labels.app=web")
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("rejects a query that is not path=value", func(t *testing.T) {
		t.Parallel()

		_, err := newTestSecretService(t, NewMockSecretRepository(t), nil).List(t.Context(), "$.labels.app")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})

	t.Run("reports a path the repository cannot parse", func(t *testing.T) {
		t.Parallel()

		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, database.ErrInvalidQueryPath).Once()

		// A bad path is the caller's mistake, so it must not surface as a server
		// failure.
		_, err := newTestSecretService(t, secrets, nil).List(t.Context(), "nonsense=web")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})
}

func TestSecretService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes a secret nothing reads", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()
		secrets.EXPECT().Delete(mock.Anything, "db-password").Return(nil).Once()

		require.NoError(t, newTestSecretService(t, secrets, nil).Delete(t.Context(), "db-password", false))
	})

	t.Run("refuses one a workload reads", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return([]string{"example"}, nil).Once()

		err := newTestSecretService(t, secrets, nil).Delete(t.Context(), "db-password", false)
		require.ErrorIs(t, err, service.ErrSecretInUse)

		// Naming the holder is the point: the alternative is an operator told only
		// that something is using it.
		assert.Contains(t, err.Error(), "example")
	})

	t.Run("removes one a workload reads when forced", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return([]string{"example"}, nil).Twice()
		secrets.EXPECT().Delete(mock.Anything, "db-password").Return(nil).Once()

		var rehashed []string
		svc := service.NewSecretService(service.SecretServiceConfig{
			Logger:  newTestLogger(t),
			Secrets: secrets,
			Cipher:  newTestCipher(t),
			Rehash: func(_ context.Context, workload string) error {
				rehashed = append(rehashed, workload)

				return nil
			},
		})

		require.NoError(t, svc.Delete(t.Context(), "db-password", true))

		// The workload is rehashed so that what it was started against stops
		// describing what orca holds.
		assert.Equal(t, []string{"example"}, rehashed)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().UsedBy(mock.Anything, "nope").Return(nil, nil).Once()
		secrets.EXPECT().Delete(mock.Anything, "nope").Return(database.ErrSecretNotFound).Once()

		err := newTestSecretService(t, secrets, nil).Delete(t.Context(), "nope", false)
		assert.ErrorIs(t, err, service.ErrSecretNotFound)
	})
}

func TestSecretService_Value(t *testing.T) {
	t.Parallel()

	t.Run("returns the plaintext", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)
		cipher := newTestCipher(t)

		sealed, err := cipher.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: sealed}, nil).Once()

		value, err := newTestSecretService(t, secrets, cipher).Value(t.Context(), "db-password")
		require.NoError(t, err)
		assert.Equal(t, "hunter2", value)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "nope").
			Return(database.Secret{}, database.ErrSecretNotFound).Once()

		value, err := newTestSecretService(t, secrets, nil).Value(t.Context(), "nope")
		require.ErrorIs(t, err, database.ErrSecretNotFound)
		assert.Empty(t, value)
	})

	t.Run("distinguishes a value it cannot decrypt", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: []byte("not openable")}, nil).Once()

		// A key that cannot decrypt what it sealed is not the same as a secret nobody
		// created, and an operator told the latter would go looking for the wrong thing.
		_, err := newTestSecretService(t, secrets, nil).Value(t.Context(), "db-password")
		require.ErrorIs(t, err, secret.ErrInvalidCiphertext)
		assert.NotErrorIs(t, err, database.ErrSecretNotFound)
	})
}

func newTestSecretService(t *testing.T, secrets *MockSecretRepository, cipher service.Cipher) *service.SecretService {
	t.Helper()

	if cipher == nil {
		cipher = newTestCipher(t)
	}

	return service.NewSecretService(service.SecretServiceConfig{
		Logger:  newTestLogger(t),
		Secrets: secrets,
		Cipher:  cipher,
		KeyID:   "test-key",
	})
}

// newTestCipher returns a real cipher rather than a mock. Encryption is the whole
// point of the type it is handed to, so a test that stubbed it out would prove
// nothing about whether a value stays sealed.
func newTestCipher(t *testing.T) *secret.Cipher {
	t.Helper()

	return newSeededCipher(t, 0)
}

// newSeededCipher returns a cipher over a key the seed distinguishes, for a test
// that needs two ciphers that are genuinely different from each other.
func newSeededCipher(t *testing.T, seed byte) *secret.Cipher {
	t.Helper()

	key := make([]byte, secret.KeyLength)
	for i := range key {
		key[i] = byte(i) + seed
	}

	c, err := secret.New(key)
	require.NoError(t, err)

	return c
}

func TestSecretService_Rekey(t *testing.T) {
	t.Parallel()

	t.Run("re-encrypts every secret under the new cipher", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)
		old, new := newSeededCipher(t, 0), newSeededCipher(t, 1)

		sealed, err := old.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		secrets.EXPECT().ListSealed(mock.Anything).
			Return([]database.Secret{{Name: "db-password", Value: sealed, KeyID: "old"}}, nil).Once()

		var written map[string][]byte
		secrets.EXPECT().Rekey(mock.Anything, "new", mock.Anything).
			RunAndReturn(func(_ context.Context, _ string, values map[string][]byte) error {
				written = values

				return nil
			}).Once()

		svc := newTestSecretService(t, secrets, old)

		count, previous, err := svc.Rekey(t.Context(), new, "new")
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Equal(t, "test-key", previous)

		// The value written has to open under the new cipher and not the old one,
		// which is the whole of what a rekey is.
		opened, err := new.Open("db-password", written["db-password"])
		require.NoError(t, err)
		assert.Equal(t, []byte("hunter2"), opened)

		_, err = old.Open("db-password", written["db-password"])
		assert.Error(t, err)
	})

	// Writing past a value that will not open would record a secret nothing can read
	// as though it had moved.
	t.Run("aborts when a value does not open", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().ListSealed(mock.Anything).
			Return([]database.Secret{{Name: "db-password", Value: []byte("not a ciphertext")}}, nil).Once()

		svc := newTestSecretService(t, secrets, nil)

		_, _, err := svc.Rekey(t.Context(), newTestCipher(t), "new")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db-password")
	})

	// The service keeps sealing under the key the database still names, or the next
	// secret set would be unreadable by the node that stored it.
	t.Run("keeps the old cipher when the rewrite fails", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)
		old := newTestCipher(t)

		secrets.EXPECT().ListSealed(mock.Anything).Return(nil, nil).Once()
		secrets.EXPECT().Rekey(mock.Anything, "new", mock.Anything).
			Return(database.ErrSecretsChanged).Once()

		svc := newTestSecretService(t, secrets, old)

		_, _, err := svc.Rekey(t.Context(), newSeededCipher(t, 1), "new")
		require.ErrorIs(t, err, database.ErrSecretsChanged)

		// Still sealing under the original key, which the stored value proves by
		// opening under it.
		sealed, err := old.Seal("db-password", []byte("hunter2"))
		require.NoError(t, err)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: sealed}, nil).Once()

		value, err := svc.Value(t.Context(), "db-password")
		require.NoError(t, err)
		assert.Equal(t, "hunter2", value)
	})
}

// TestSecretService_RekeyIsAtomicForReaders covers the property the lock exists for.
// A reader running while a rekey is in flight sees the values and the cipher from the
// same side of it, never a value sealed under one key opened with another.
//
// Run under -race, this also proves the cipher swap is not a data race against the
// readers.
func TestSecretService_RekeyIsAtomicForReaders(t *testing.T) {
	t.Parallel()

	old, new := newSeededCipher(t, 0), newSeededCipher(t, 1)

	sealedOld, err := old.Seal("db-password", []byte("hunter2"))
	require.NoError(t, err)
	sealedNew, err := new.Seal("db-password", []byte("hunter2"))
	require.NoError(t, err)

	secrets := NewMockSecretRepository(t)
	secrets.EXPECT().ListSealed(mock.Anything).
		Return([]database.Secret{{Name: "db-password", Value: sealedOld}}, nil).Once()

	// The repository flips to the resealed value when the rewrite commits, which is
	// what a reader on the other side of the transaction would see.
	rewritten := make(chan struct{})
	secrets.EXPECT().Rekey(mock.Anything, "new", mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, _ map[string][]byte) error {
			close(rewritten)

			return nil
		}).Once()

	secrets.EXPECT().Get(mock.Anything, "db-password").
		RunAndReturn(func(_ context.Context, _ string) (database.Secret, error) {
			select {
			case <-rewritten:
				return database.Secret{Name: "db-password", Value: sealedNew}, nil
			default:
				return database.Secret{Name: "db-password", Value: sealedOld}, nil
			}
		})

	svc := newTestSecretService(t, secrets, old)

	var wg sync.WaitGroup

	for range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// Whichever side of the rekey this lands on, the value has to come back
			// intact. A read that straddled it would fail to decrypt.
			value, err := svc.Value(t.Context(), "db-password")
			assert.NoError(t, err)
			assert.Equal(t, "hunter2", value)
		}()
	}

	_, _, err = svc.Rekey(t.Context(), new, "new")
	require.NoError(t, err)

	wg.Wait()
}
