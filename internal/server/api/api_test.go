package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Logger:    logger,
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

func TestRegister_RequestErrors(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Target       string
		Body         string
		ExpectStatus int
		ExpectError  string
	}{
		{
			Name:         "a body that is not JSON is a 400 in the error shape",
			Target:       "/api/v1/workloads/example",
			Body:         "not json",
			ExpectStatus: http.StatusBadRequest,
			ExpectError:  "can't decode JSON body",
		},
		{
			Name:         "a body over the limit is a 413 in the error shape",
			Target:       "/api/v1/workloads/example",
			Body:         `{"name":"` + strings.Repeat("a", 2<<20),
			ExpectStatus: http.StatusRequestEntityTooLarge,
			ExpectError:  "the request body is larger than",
		},
		{
			Name:         "a parameter that does not parse is a 400 in the error shape",
			Target:       "/api/v1/workloads/example?force=maybe",
			ExpectStatus: http.StatusBadRequest,
			ExpectError:  "force",
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

			mux := http.NewServeMux()
			api.New(api.Config{
				Logger:    logger,
				Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
			}).Register(mux)

			method := http.MethodDelete
			var body io.Reader
			if tc.Body != "" {
				method = http.MethodPut
				body = strings.NewReader(tc.Body)
			}

			req := httptest.NewRequest(method, tc.Target, body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			middleware.Limit(mux).ServeHTTP(resp, req)
			require.Equal(t, tc.ExpectStatus, resp.Code)
			assert.Equal(t, "application/json", resp.Header().Get("Content-Type"))

			var answer api.ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&answer))
			assert.Contains(t, answer.Message, tc.ExpectError)
		})
	}
}
