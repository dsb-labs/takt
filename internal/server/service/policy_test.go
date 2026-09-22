package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

func newTestPolicyService(t *testing.T, policies service.PolicyRepository) *service.PolicyService {
	t.Helper()

	return service.NewPolicyService(service.PolicyServiceConfig{
		Logger:   newTestLogger(t),
		Policies: policies,
	})
}

func TestPolicyService_Get(t *testing.T) {
	t.Parallel()

	// Before any apply there is no row, and version zero is how a caller names
	// that state when it goes on to apply against it.
	t.Run("reports the empty policy at version zero before any apply", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Get(mock.Anything).Return(database.Policy{}, database.ErrNoPolicy).Once()

		policy, err := newTestPolicyService(t, policies).Get(t.Context())
		require.NoError(t, err)
		assert.Equal(t, manifest.Policy{Version: "v1"}, policy.Spec)
		assert.Equal(t, 0, policy.Version)
	})

	t.Run("reports the stored policy and its version", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Get(mock.Anything).Return(database.Policy{
			Document: []byte(`{"version":"v1","grants":[{"principals":["prometheus"],"role":"viewer"}]}`),
			Version:  3,
		}, nil).Once()

		policy, err := newTestPolicyService(t, policies).Get(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 3, policy.Version)
		require.Len(t, policy.Spec.Grants, 1)
		assert.Equal(t, manifest.RoleViewer, policy.Spec.Grants[0].Role)
	})
}

func TestPolicyService_Apply(t *testing.T) {
	t.Parallel()

	valid := manifest.Policy{
		Version: "v1",
		Grants: []manifest.PolicyGrant{
			{Principals: []string{"prometheus"}, Role: manifest.RoleViewer},
		},
	}

	// The canonical form of valid, which is what reaches the repository and
	// what it hands back.
	document := []byte(`{"version":"v1","grants":[{"principals":["prometheus"],"role":"viewer"}]}`)

	t.Run("applies a document against the stored version", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, 1).
			Return(database.Policy{Document: document, Version: 2}, nil).Once()

		applied, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, 1)
		require.NoError(t, err)
		assert.Equal(t, valid, applied.Spec)
		assert.Equal(t, 2, applied.Version)
	})

	t.Run("applies the first document against version zero", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, 0).
			Return(database.Policy{Document: document, Version: 1}, nil).Once()

		applied, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, 0)
		require.NoError(t, err)
		assert.Equal(t, 1, applied.Version)
	})

	t.Run("reports a stale version", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, 1).
			Return(database.Policy{}, database.ErrPolicyChanged).Once()

		_, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, 1)
		assert.ErrorIs(t, err, service.ErrPolicyChanged)
	})

	t.Run("refuses an invalid document", func(t *testing.T) {
		invalid := manifest.Policy{
			Version: "v1",
			Grants:  []manifest.PolicyGrant{{Principals: []string{"someone"}, Role: "root"}},
		}

		_, err := newTestPolicyService(t, NewMockPolicyRepository(t)).Apply(t.Context(), invalid, 1)
		assert.ErrorIs(t, err, service.ErrInvalidPolicy)
	})

	t.Run("hands the repository the canonical document", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, document, 1).
			Return(database.Policy{Document: document, Version: 2}, nil).Once()

		_, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, 1)
		require.NoError(t, err)
	})
}
