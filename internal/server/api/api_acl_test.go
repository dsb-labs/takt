package api_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// grantViewer is the policy the acl tests move around.
var grantViewer = manifest.Policy{
	Version: "v1",
	Grants: []manifest.PolicyGrant{
		{Principals: []string{"prometheus"}, Role: manifest.RoleViewer},
	},
}

func doACL(t *testing.T, policies api.PolicyService, init api.ACLInitializer, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		ACL: api.NewACLAPI(api.ACLAPIConfig{Logger: logger, Policies: policies, Init: init}),
	}).Register(mux)

	resp := httptest.NewRecorder()
	// Served through the disabled-mode authenticate middleware, so the
	// authorize layer passes the request; its own refusals are tested in
	// api_test.go.
	middleware.Authenticate(nil)(mux).ServeHTTP(resp, req)

	return resp
}

func TestACLAPI_GetACLPolicy(t *testing.T) {
	t.Parallel()

	t.Run("returns the policy with its tag", func(t *testing.T) {
		policies := NewMockPolicyService(t)
		policies.EXPECT().Get(mock.Anything).Return(grantViewer, "tag-1", nil).Once()

		resp := doACL(t, policies, NewMockACLInitializer(t),
			httptest.NewRequest(http.MethodGet, "/api/v1/acl", nil))

		require.Equal(t, http.StatusOK, resp.Code)
		assert.Equal(t, `"tag-1"`, resp.Header().Get("ETag"))
		assert.JSONEq(t, `{
			"policy": {
				"version": "v1",
				"grants": [{"principals": ["prometheus"], "role": "viewer"}]
			}
		}`, resp.Body.String())
	})

	t.Run("scrubs an internal failure", func(t *testing.T) {
		policies := NewMockPolicyService(t)
		policies.EXPECT().Get(mock.Anything).Return(manifest.Policy{}, "", assert.AnError).Once()

		resp := doACL(t, policies, NewMockACLInitializer(t),
			httptest.NewRequest(http.MethodGet, "/api/v1/acl", nil))

		require.Equal(t, http.StatusInternalServerError, resp.Code)
		assert.NotContains(t, resp.Body.String(), assert.AnError.Error())
	})
}

func TestACLAPI_ApplyACLPolicy(t *testing.T) {
	t.Parallel()

	document := `{"version": "v1", "grants": [{"principals": ["prometheus"], "role": "viewer"}]}`

	apply := func(t *testing.T, policies api.PolicyService, ifMatch string) *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequest(http.MethodPut, "/api/v1/acl", strings.NewReader(document))
		req.Header.Set("Content-Type", "application/json")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}

		return doACL(t, policies, NewMockACLInitializer(t), req)
	}

	t.Run("replaces the policy against the stored tag", func(t *testing.T) {
		policies := NewMockPolicyService(t)
		// The quotes the header carries are stripped before the service sees
		// the tag.
		policies.EXPECT().Apply(mock.Anything, grantViewer, "tag-1").
			Return(grantViewer, "tag-2", nil).Once()

		resp := apply(t, policies, `"tag-1"`)
		require.Equal(t, http.StatusOK, resp.Code)
		assert.Equal(t, `"tag-2"`, resp.Header().Get("ETag"))
	})

	t.Run("accepts a bare tag without quotes", func(t *testing.T) {
		policies := NewMockPolicyService(t)
		policies.EXPECT().Apply(mock.Anything, grantViewer, "tag-1").
			Return(grantViewer, "tag-2", nil).Once()

		resp := apply(t, policies, "tag-1")
		require.Equal(t, http.StatusOK, resp.Code)
	})

	t.Run("refuses a stale tag", func(t *testing.T) {
		policies := NewMockPolicyService(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, "stale").
			Return(manifest.Policy{}, "", service.ErrPolicyChanged).Once()

		resp := apply(t, policies, "stale")
		require.Equal(t, http.StatusPreconditionFailed, resp.Code)
	})

	t.Run("refuses an invalid document", func(t *testing.T) {
		policies := NewMockPolicyService(t)
		policies.EXPECT().Apply(mock.Anything, mock.Anything, "tag-1").
			Return(manifest.Policy{}, "", service.ErrInvalidPolicy).Once()

		resp := apply(t, policies, "tag-1")
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})

	t.Run("refuses an apply without the If-Match header", func(t *testing.T) {
		resp := apply(t, NewMockPolicyService(t), "")
		require.Equal(t, http.StatusBadRequest, resp.Code)
	})
}

func TestACLAPI_InitACL(t *testing.T) {
	t.Parallel()

	t.Run("mints the recovery token once", func(t *testing.T) {
		init := NewMockACLInitializer(t)
		init.EXPECT().Init(mock.Anything).Return("takt_r_secret", nil).Once()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/acl/init", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")

		resp := doACL(t, NewMockPolicyService(t), init, req)
		require.Equal(t, http.StatusCreated, resp.Code)
		assert.JSONEq(t, `{"credential": "takt_r_secret"}`, resp.Body.String())
	})

	t.Run("refuses a second init", func(t *testing.T) {
		init := NewMockACLInitializer(t)
		init.EXPECT().Init(mock.Anything).Return("", service.ErrACLInitialized).Once()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/acl/init", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")

		resp := doACL(t, NewMockPolicyService(t), init, req)
		require.Equal(t, http.StatusConflict, resp.Code)
	})
}
