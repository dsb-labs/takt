package service_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/service"
	"github.com/dsb-labs/orca/pkg/manifest"
)

func TestEnvResolver_Resolve(t *testing.T) {
	t.Parallel()

	t.Run("substitutes the secrets a workload reads", func(t *testing.T) {
		secrets := NewMockValueStore(t)

		secrets.EXPECT().Value(mock.Anything, "db-password").Return("hunter2", nil).Once()

		resolved, err := newTestEnvResolver(t, secrets, nil).Resolve(t.Context(), map[string]string{
			"DSN":     "postgres://app:${secret:db-password}@localhost/app",
			"LITERAL": "$$notasecret",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "postgres://app:hunter2@localhost/app", resolved["DSN"])
		assert.Equal(t, "$notasecret", resolved["LITERAL"])
	})

	t.Run("substitutes the variables a workload reads", func(t *testing.T) {
		variables := NewMockValueStore(t)

		variables.EXPECT().Value(mock.Anything, "db-host").Return("localhost", nil).Once()

		resolved, err := newTestEnvResolver(t, nil, variables).Resolve(t.Context(), map[string]string{
			"DSN": "postgres://app@${var:db-host}/app",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "postgres://app@localhost/app", resolved["DSN"])
	})

	t.Run("substitutes both kinds in one value", func(t *testing.T) {
		secrets, variables := NewMockValueStore(t), NewMockValueStore(t)

		secrets.EXPECT().Value(mock.Anything, "db-password").Return("hunter2", nil).Once()
		variables.EXPECT().Value(mock.Anything, "db-host").Return("localhost", nil).Once()

		// One pass over the value, because expansion refuses what it cannot resolve. A
		// pass that saw only one kind would have to treat the other as unresolvable.
		resolved, err := newTestEnvResolver(t, secrets, variables).Resolve(t.Context(), map[string]string{
			"DSN": "postgres://app:${secret:db-password}@${var:db-host}/app",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "postgres://app:hunter2@localhost/app", resolved["DSN"])
	})

	t.Run("reads a secret once however many variables reference it", func(t *testing.T) {
		secrets := NewMockValueStore(t)

		secrets.EXPECT().Value(mock.Anything, "token").Return("abc", nil).Once()

		resolved, err := newTestEnvResolver(t, secrets, nil).Resolve(t.Context(), map[string]string{
			"ONE": "${secret:token}",
			"TWO": "${secret:token}",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "abc", resolved["ONE"])
		assert.Equal(t, "abc", resolved["TWO"])
	})

	t.Run("reads a variable once however many reference it", func(t *testing.T) {
		variables := NewMockValueStore(t)

		variables.EXPECT().Value(mock.Anything, "region").Return("eu-west", nil).Once()

		resolved, err := newTestEnvResolver(t, nil, variables).Resolve(t.Context(), map[string]string{
			"ONE": "${var:region}",
			"TWO": "${var:region}",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "eu-west", resolved["ONE"])
		assert.Equal(t, "eu-west", resolved["TWO"])
	})

	t.Run("tells a secret and a variable of the same name apart", func(t *testing.T) {
		secrets, variables := NewMockValueStore(t), NewMockValueStore(t)

		secrets.EXPECT().Value(mock.Anything, "token").Return("private", nil).Once()
		variables.EXPECT().Value(mock.Anything, "token").Return("public", nil).Once()

		// The memo is keyed by the reference rather than the name, so a name held by
		// both is read from both rather than answered once for whichever came first.
		resolved, err := newTestEnvResolver(t, secrets, variables).Resolve(t.Context(), map[string]string{
			"SECRET": "${secret:token}",
			"PUBLIC": "${var:token}",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "private", resolved["SECRET"])
		assert.Equal(t, "public", resolved["PUBLIC"])
	})

	t.Run("leaves an environment referencing nothing alone", func(t *testing.T) {
		env := map[string]string{"PLAIN": "value"}

		resolved, err := newTestEnvResolver(t, nil, nil).Resolve(t.Context(), env, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, env, resolved)
	})

	t.Run("reports a secret that does not exist", func(t *testing.T) {
		secrets := NewMockValueStore(t)

		secrets.EXPECT().Value(mock.Anything, "nope").
			Return("", fmt.Errorf("%w: nope", service.ErrSecretNotFound)).Once()

		// Handing the workload the reference text would have it use that as the value.
		_, err := newTestEnvResolver(t, secrets, nil).
			Resolve(t.Context(), map[string]string{"DSN": "${secret:nope}"}, "reader", 0)
		require.ErrorIs(t, err, manifest.ErrUnknownSecret)

		// Both ends of the reference, so an operator knows which variable to look at as
		// well as what it could not read.
		assert.Contains(t, err.Error(), "DSN")
		assert.Contains(t, err.Error(), "nope")
	})

	t.Run("reports a variable that does not exist", func(t *testing.T) {
		variables := NewMockValueStore(t)

		variables.EXPECT().Value(mock.Anything, "nope").
			Return("", fmt.Errorf("%w: nope", service.ErrVariableNotFound)).Once()

		_, err := newTestEnvResolver(t, nil, variables).
			Resolve(t.Context(), map[string]string{"LEVEL": "${var:nope}"}, "reader", 0)
		require.ErrorIs(t, err, manifest.ErrUnknownVariable)
		assert.Contains(t, err.Error(), "LEVEL")
		assert.Contains(t, err.Error(), "nope")
	})

	t.Run("distinguishes a read that failed from one that found nothing", func(t *testing.T) {
		secrets := NewMockValueStore(t)

		failure := errors.New("database is gone")
		secrets.EXPECT().Value(mock.Anything, "db-password").Return("", failure).Once()

		// A read that failed is not the same as a secret nobody created, and an
		// operator told the latter would go looking for the wrong thing.
		_, err := newTestEnvResolver(t, secrets, nil).
			Resolve(t.Context(), map[string]string{"DSN": "${secret:db-password}"}, "reader", 0)
		require.ErrorIs(t, err, failure)
		assert.NotErrorIs(t, err, manifest.ErrUnknownSecret)
	})

	t.Run("reports a malformed reference", func(t *testing.T) {
		_, err := newTestEnvResolver(t, NewMockValueStore(t), nil).
			Resolve(t.Context(), map[string]string{"DSN": "${secret:unterminated"}, "reader", 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DSN")
	})

	t.Run("refuses a secret on a server holding none", func(t *testing.T) {
		// A resolver with no secret store cannot resolve one, and a workload handed
		// the reference text would use it as the value.
		_, err := newTestEnvResolver(t, nil, NewMockValueStore(t)).
			Resolve(t.Context(), map[string]string{"DSN": "${secret:db-password}"}, "reader", 0)
		assert.ErrorIs(t, err, manifest.ErrUnknownSecret)
	})

	t.Run("refuses a variable on a server holding none", func(t *testing.T) {
		_, err := newTestEnvResolver(t, NewMockValueStore(t), nil).
			Resolve(t.Context(), map[string]string{"LEVEL": "${var:log-level}"}, "reader", 0)
		assert.ErrorIs(t, err, manifest.ErrUnknownVariable)
	})

	t.Run("substitutes the address of a referenced workload", func(t *testing.T) {
		addresses := NewMockAddressResolver(t)

		addresses.EXPECT().
			Address(mock.Anything, manifest.Reference{Kind: manifest.KindWorkload, Name: "postgres", Port: "pg"}, "reader", 0).
			Return("10.0.0.5:20432", nil).Once()

		resolved, err := newTestEnvResolverWithAddresses(t, addresses).Resolve(t.Context(), map[string]string{
			"DSN": "postgres://app@${workload:postgres:pg}/app",
		}, "reader", 0)
		require.NoError(t, err)
		assert.Equal(t, "postgres://app@10.0.0.5:20432/app", resolved["DSN"])
	})

	t.Run("reports a workload that does not exist", func(t *testing.T) {
		addresses := NewMockAddressResolver(t)

		addresses.EXPECT().Address(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return("", fmt.Errorf("%w: nope", service.ErrWorkloadNotFound)).Once()

		// The paced restart retries until the workload exists, so this is what a
		// consumer waiting on its dependency reports in the meantime.
		_, err := newTestEnvResolverWithAddresses(t, addresses).
			Resolve(t.Context(), map[string]string{"DSN": "${workload:nope}"}, "reader", 0)
		require.ErrorIs(t, err, manifest.ErrUnknownWorkload)
		assert.Contains(t, err.Error(), "DSN")
		assert.Contains(t, err.Error(), "nope")
	})

	t.Run("reports a port the referenced workload does not publish", func(t *testing.T) {
		addresses := NewMockAddressResolver(t)

		failure := fmt.Errorf("%w: workload postgres does not publish http", service.ErrPortNotPublished)
		addresses.EXPECT().Address(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("", failure).Once()

		// Not the same as a workload nobody created. The workload is right there, and
		// an operator told its address is unknown would go looking for the wrong
		// thing.
		_, err := newTestEnvResolverWithAddresses(t, addresses).
			Resolve(t.Context(), map[string]string{"DSN": "${workload:postgres:http}"}, "reader", 0)
		require.ErrorIs(t, err, failure)
		assert.NotErrorIs(t, err, manifest.ErrUnknownWorkload)
	})

	t.Run("refuses a workload reference on a server resolving none", func(t *testing.T) {
		_, err := newTestEnvResolver(t, nil, nil).
			Resolve(t.Context(), map[string]string{"DSN": "${workload:postgres}"}, "reader", 0)
		assert.ErrorIs(t, err, manifest.ErrUnknownWorkload)
	})
}

func newTestEnvResolverWithAddresses(t *testing.T, addresses service.AddressResolver) *service.EnvResolver {
	t.Helper()

	return service.NewEnvResolver(service.EnvResolverConfig{
		Logger:    newTestLogger(t),
		Workloads: addresses,
	})
}

func newTestEnvResolver(t *testing.T, secrets, variables service.ValueStore) *service.EnvResolver {
	t.Helper()

	return service.NewEnvResolver(service.EnvResolverConfig{
		Logger:    newTestLogger(t),
		Secrets:   secrets,
		Variables: variables,
	})
}
