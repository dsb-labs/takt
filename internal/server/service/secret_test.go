package service_test

import (
	"context"
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
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, value []byte, revision, _ string) (database.Secret, error) {
				// What reaches the database is the sealed value, never the plaintext.
				assert.NotContains(t, string(value), "hunter2")

				return database.Secret{Name: name, Value: value, Revision: revision}, nil
			}).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		stored, created, err := newTestSecretService(t, secrets, nil).
			Set(t.Context(), "db-password", []byte("hunter2"))
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
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, name string, value []byte, rev, _ string) (database.Secret, error) {
				revision = rev

				return database.Secret{Name: name, Value: value, Revision: rev}, nil
			}).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		stored, created, err := newTestSecretService(t, secrets, cipher).
			Set(t.Context(), "db-password", []byte("hunter3"))
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
			Set(t.Context(), "db-password", []byte("hunter2"))
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, "rev-one", stored.Revision)
	})

	t.Run("redeploys the workloads reading it", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{}, database.ErrSecretNotFound).Once()
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything).
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

		_, _, err := svc.Set(t.Context(), "db-password", []byte("hunter2"))
		require.NoError(t, err)
		assert.Equal(t, []string{"one", "two"}, rehashed)
	})

	t.Run("refuses a name orca would not accept", func(t *testing.T) {
		_, _, err := newTestSecretService(t, NewMockSecretRepository(t), nil).
			Set(t.Context(), "DB_PASSWORD", []byte("hunter2"))
		assert.ErrorIs(t, err, service.ErrInvalidSecret)
	})

	t.Run("re-seals a value it cannot open", func(t *testing.T) {
		secrets := NewMockSecretRepository(t)

		secrets.EXPECT().Get(mock.Anything, "db-password").
			Return(database.Secret{Name: "db-password", Value: []byte("sealed under a key that is gone")}, nil).Once()
		secrets.EXPECT().Upsert(mock.Anything, "db-password", mock.Anything, mock.Anything, mock.Anything).
			Return(database.Secret{Name: "db-password", Revision: "rev-two"}, nil).Once()
		secrets.EXPECT().UsedBy(mock.Anything, "db-password").Return(nil, nil).Once()

		// A key rotated out from under the database leaves a value nothing can read.
		// Re-sealing under the current key is more useful than refusing to move.
		_, _, err := newTestSecretService(t, secrets, nil).Set(t.Context(), "db-password", []byte("hunter2"))
		require.NoError(t, err)
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
		require.ErrorIs(t, err, service.ErrSecretNotFound)
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
		assert.NotErrorIs(t, err, service.ErrSecretNotFound)
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
	})
}

// newTestCipher returns a real cipher rather than a mock. Encryption is the whole
// point of the type it is handed to, so a test that stubbed it out would prove
// nothing about whether a value stays sealed.
func newTestCipher(t *testing.T) *secret.Cipher {
	t.Helper()

	key := make([]byte, secret.KeyLength)
	for i := range key {
		key[i] = byte(i)
	}

	c, err := secret.New(key)
	require.NoError(t, err)

	return c
}
