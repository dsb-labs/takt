package api_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/api"
	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// doAuthorized serves req as the given caller, with the workload and token
// services mocked to succeed, so what varies between cases is only whether
// the authorize layer lets the request reach them.
func doAuthorized(t *testing.T, authenticator middleware.Authenticator, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	workloads := NewMockWorkloadService(t)
	workloads.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()

	tokens := NewMockTokenService(t)
	tokens.EXPECT().List(mock.Anything).Return(nil, nil).Maybe()

	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: workloads}),
		Tokens:    api.NewTokenAPI(api.TokenAPIConfig{Logger: logger, Tokens: tokens}),
		Auth:      api.NewAuthAPI(api.AuthAPIConfig{Logger: logger, Auth: NewMockAuthService(t)}),
	}).Register(mux)

	resp := httptest.NewRecorder()
	middleware.Authenticate(authenticator)(mux).ServeHTTP(resp, req)

	return resp
}

func TestAuthorize(t *testing.T) {
	t.Parallel()

	// The routes stand in for the three requirement shapes the document
	// declares: a role, authentication without a role, and anonymity.
	const (
		viewerRoute = "/api/v1/workloads"
		adminRoute  = "/api/v1/tokens"
		whoamiRoute = "/api/v1/auth"
	)

	as := func(identity auth.Identity) middleware.Authenticator {
		return identityStub{identity: identity}
	}

	tt := []struct {
		Name          string
		Authenticator middleware.Authenticator
		Target        string
		ExpectStatus  int
	}{
		{
			Name:          "a viewer reads",
			Authenticator: as(auth.Identity{Principal: "prometheus", Role: manifest.RoleViewer, TokenID: "id"}),
			Target:        viewerRoute,
			ExpectStatus:  http.StatusOK,
		},
		{
			Name:          "a viewer is refused an admin operation",
			Authenticator: as(auth.Identity{Principal: "prometheus", Role: manifest.RoleViewer, TokenID: "id"}),
			Target:        adminRoute,
			ExpectStatus:  http.StatusForbidden,
		},
		{
			// The roles are hierarchical, so an admin passes a viewer
			// requirement without a grant naming viewer.
			Name:          "an admin reads",
			Authenticator: as(auth.Identity{Principal: "david@dsb.dev", Role: manifest.RoleAdmin, TokenID: "id"}),
			Target:        viewerRoute,
			ExpectStatus:  http.StatusOK,
		},
		{
			Name:          "an admin manages tokens",
			Authenticator: as(auth.Identity{Principal: "david@dsb.dev", Role: manifest.RoleAdmin, TokenID: "id"}),
			Target:        adminRoute,
			ExpectStatus:  http.StatusOK,
		},
		{
			// A principal with a token but no grant is a real state, and it
			// holds nothing but whoami.
			Name:          "a principal granted nothing is refused",
			Authenticator: as(auth.Identity{Principal: "stranger", TokenID: "id"}),
			Target:        viewerRoute,
			ExpectStatus:  http.StatusForbidden,
		},
		{
			Name:          "a principal granted nothing still sees who it is",
			Authenticator: as(auth.Identity{Principal: "stranger", TokenID: "id"}),
			Target:        whoamiRoute,
			ExpectStatus:  http.StatusOK,
		},
		{
			Name:          "an anonymous caller is refused a secured operation",
			Authenticator: as(auth.Identity{}),
			Target:        viewerRoute,
			ExpectStatus:  http.StatusUnauthorized,
		},
		{
			Name:          "an anonymous caller is refused whoami",
			Authenticator: as(auth.Identity{}),
			Target:        whoamiRoute,
			ExpectStatus:  http.StatusUnauthorized,
		},
		{
			// The recovery token sits above policy, which is what makes a bad
			// policy apply recoverable.
			Name:          "the recovery token passes everything",
			Authenticator: as(auth.Identity{Role: manifest.RoleAdmin, TokenID: "id", Recovery: true}),
			Target:        adminRoute,
			ExpectStatus:  http.StatusOK,
		},
		{
			Name:         "the disabled mode passes everything",
			Target:       adminRoute,
			ExpectStatus: http.StatusOK,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.Target, nil)
			if tc.Authenticator != nil {
				// The stub answers for any credential; the header only has to
				// present one.
				req.Header.Set("Authorization", "Bearer anything")
			}

			resp := doAuthorized(t, tc.Authenticator, req)
			require.Equal(t, tc.ExpectStatus, resp.Code)

			if tc.ExpectStatus == http.StatusUnauthorized {
				assert.Equal(t, "Bearer", resp.Header().Get("WWW-Authenticate"))
			}
		})
	}
}
