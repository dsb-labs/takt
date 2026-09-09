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

	t.Run("reports the empty policy before any apply", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Get(mock.Anything).Return(database.Policy{}, database.ErrNoPolicy).Once()

		policy, etag, err := newTestPolicyService(t, policies).Get(t.Context())
		require.NoError(t, err)
		assert.Equal(t, manifest.Policy{Version: "v1"}, policy)
		assert.NotEmpty(t, etag)
	})

	t.Run("reports the stored policy and its tag", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Get(mock.Anything).Return(database.Policy{
			Document: []byte(`{"version":"v1","grants":[{"principals":["prometheus"],"role":"viewer"}]}`),
			ETag:     "tag-1",
		}, nil).Once()

		policy, etag, err := newTestPolicyService(t, policies).Get(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "tag-1", etag)
		require.Len(t, policy.Grants, 1)
		assert.Equal(t, manifest.RoleViewer, policy.Grants[0].Role)
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

	t.Run("applies a document against the stored tag", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything, "tag-1").Return(nil).Once()

		applied, etag, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, "tag-1")
		require.NoError(t, err)
		assert.Equal(t, valid, applied)
		assert.NotEmpty(t, etag)
	})

	// The empty document is never stored, so a caller conditioning on its tag
	// is asking to replace the state where no row exists yet.
	t.Run("maps the empty policy's tag to the first apply", func(t *testing.T) {
		empty := NewMockPolicyRepository(t)
		empty.EXPECT().Get(mock.Anything).Return(database.Policy{}, database.ErrNoPolicy).Once()

		_, emptyETag, err := newTestPolicyService(t, empty).Get(t.Context())
		require.NoError(t, err)

		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything, "").Return(nil).Once()

		_, _, err = newTestPolicyService(t, policies).Apply(t.Context(), valid, emptyETag)
		require.NoError(t, err)
	})

	t.Run("reports a stale tag", func(t *testing.T) {
		policies := NewMockPolicyRepository(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything, "stale").
			Return(database.ErrPolicyChanged).Once()

		_, _, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, "stale")
		assert.ErrorIs(t, err, service.ErrPolicyChanged)
	})

	t.Run("refuses an invalid document", func(t *testing.T) {
		invalid := manifest.Policy{
			Version: "v1",
			Grants:  []manifest.PolicyGrant{{Principals: []string{"someone"}, Role: "root"}},
		}

		_, _, err := newTestPolicyService(t, NewMockPolicyRepository(t)).Apply(t.Context(), invalid, "tag-1")
		assert.ErrorIs(t, err, service.ErrInvalidPolicy)
	})

	t.Run("derives the same tag for the same document", func(t *testing.T) {
		var tags []string
		for range 2 {
			policies := NewMockPolicyRepository(t)
			policies.EXPECT().Apply(mock.Anything, mock.Anything, mock.Anything, "tag-1").Return(nil).Once()

			_, etag, err := newTestPolicyService(t, policies).Apply(t.Context(), valid, "tag-1")
			require.NoError(t, err)

			tags = append(tags, etag)
		}

		assert.Equal(t, tags[0], tags[1])
	})
}
