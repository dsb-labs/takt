package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// The authenticatorFunc type adapts a function to the Authenticator
// interface, which is all these tests need of one.
type authenticatorFunc func(ctx context.Context, credential string) (auth.Identity, error)

func (fn authenticatorFunc) Authenticate(ctx context.Context, credential string) (auth.Identity, error) {
	return fn(ctx, credential)
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()

	identified := auth.Identity{Principal: "prometheus", Role: manifest.RoleViewer, TokenID: "id"}

	resolve := authenticatorFunc(func(_ context.Context, credential string) (auth.Identity, error) {
		if credential == "valid" {
			return identified, nil
		}

		return auth.Identity{}, service.ErrInvalidCredential
	})

	tt := []struct {
		Name          string
		Authenticator middleware.Authenticator
		Header        string
		Cookie        string
		ExpectStatus  int
		ExpectCaller  auth.Identity
	}{
		{
			Name:          "resolves a bearer credential",
			Authenticator: resolve,
			Header:        "Bearer valid",
			ExpectStatus:  http.StatusOK,
			ExpectCaller:  identified,
		},
		{
			Name:          "resolves a session cookie",
			Authenticator: resolve,
			Cookie:        "valid",
			ExpectStatus:  http.StatusOK,
			ExpectCaller:  identified,
		},
		{
			// The bearer header wins over the cookie, so a CLI call from a
			// browser-adjacent context is judged by what it presented.
			Name:          "prefers the bearer header over the cookie",
			Authenticator: resolve,
			Header:        "Bearer valid",
			Cookie:        "revoked",
			ExpectStatus:  http.StatusOK,
			ExpectCaller:  identified,
		},
		{
			Name:          "refuses a credential that does not authenticate",
			Authenticator: resolve,
			Header:        "Bearer revoked",
			ExpectStatus:  http.StatusUnauthorized,
		},
		{
			// Absence is not refusal. Whether anonymity is enough is each
			// operation's declared requirement, and the health probes depend
			// on that distinction.
			Name:          "passes a request presenting nothing as anonymous",
			Authenticator: resolve,
			ExpectStatus:  http.StatusOK,
			ExpectCaller:  auth.Identity{},
		},
		{
			Name:          "ignores a non-bearer authorization header",
			Authenticator: resolve,
			Header:        "Basic dXNlcjpwYXNz",
			ExpectStatus:  http.StatusOK,
			ExpectCaller:  auth.Identity{},
		},
		{
			Name:         "passes everything as disabled without an authenticator",
			Header:       "Bearer anything",
			ExpectStatus: http.StatusOK,
			ExpectCaller: auth.Identity{Disabled: true},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			var caller auth.Identity

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				caller = middleware.CallerIdentity(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
			if tc.Header != "" {
				req.Header.Set("Authorization", tc.Header)
			}

			if tc.Cookie != "" {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: tc.Cookie})
			}

			resp := httptest.NewRecorder()
			middleware.Authenticate(tc.Authenticator)(handler).ServeHTTP(resp, req)

			require.Equal(t, tc.ExpectStatus, resp.Code)
			if tc.ExpectStatus == http.StatusOK {
				assert.Equal(t, tc.ExpectCaller, caller)

				return
			}

			assert.Equal(t, "Bearer", resp.Header().Get("WWW-Authenticate"))
			assert.JSONEq(t, `{"error": "invalid credential"}`, resp.Body.String())
		})
	}
}

func TestCallerIdentity(t *testing.T) {
	t.Parallel()

	// A handler served without the middleware reads the anonymous identity,
	// which refuses everything an operation requires a role for.
	assert.Zero(t, middleware.CallerIdentity(t.Context()))
}
