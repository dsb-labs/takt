package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/internal/server/middleware"
)

func TestRequireJSON(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Method       string
		ContentType  string
		NoBody       bool
		ExpectStatus int
	}{
		{
			Name:         "accepts a json body",
			Method:       http.MethodPut,
			ContentType:  "application/json",
			ExpectStatus: http.StatusOK,
		},
		{
			Name:         "accepts a json body declaring a charset",
			Method:       http.MethodPut,
			ContentType:  "application/json; charset=utf-8",
			ExpectStatus: http.StatusOK,
		},
		{
			Name: "refuses a plain text body",
			// One of the content types a browser sends across origins without asking
			// permission first, which is what makes requiring json worth doing.
			Method:       http.MethodPost,
			ContentType:  "text/plain",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "refuses a form encoded body",
			Method:       http.MethodPost,
			ContentType:  "application/x-www-form-urlencoded",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "refuses a body declaring nothing",
			Method:       http.MethodPut,
			ContentType:  "",
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "lets a request with no body through",
			Method:       http.MethodGet,
			ExpectStatus: http.StatusOK,
		},
		{
			// Every write this API serves carries a JSON object, including a rekey,
			// which sends an empty one. A bodyless POST is therefore never a request
			// this API meant to serve, and it is one a browser may send across
			// origins without asking permission first.
			Name:         "refuses a post with no body",
			Method:       http.MethodPost,
			NoBody:       true,
			ExpectStatus: http.StatusUnsupportedMediaType,
		},
		{
			Name:         "lets a delete through",
			Method:       http.MethodDelete,
			ExpectStatus: http.StatusOK,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			body := "{}"
			if tc.NoBody {
				body = ""
			}

			req := httptest.NewRequest(tc.Method, "/api/v1/workloads/example", strings.NewReader(body))
			if tc.ContentType != "" {
				req.Header.Set("Content-Type", tc.ContentType)
			}

			resp := httptest.NewRecorder()

			middleware.RequireJSON(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(resp, req)

			assert.Equal(t, tc.ExpectStatus, resp.Code)
		})
	}
}
