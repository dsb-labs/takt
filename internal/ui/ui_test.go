package ui_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"

	"github.com/dsb-labs/takt/internal/ui"
)

func TestNewHandler(t *testing.T) {
	t.Parallel()

	bundle := fstest.MapFS{
		"index.html":         {Data: []byte("<html>index</html>")},
		"assets/app.abc.js":  {Data: []byte("js")},
		"assets/app.abc.css": {Data: []byte("css")},
		"takt.svg":           {Data: []byte("svg")},
	}

	tt := []struct {
		Name            string
		Files           fstest.MapFS
		Path            string
		ExpectedStatus  int
		ExpectedBody    string
		ExpectedCaching string
	}{
		{
			Name:            "root serves the index page",
			Files:           bundle,
			Path:            "/",
			ExpectedStatus:  http.StatusOK,
			ExpectedBody:    "<html>index</html>",
			ExpectedCaching: "no-cache",
		},
		{
			Name:            "unknown path falls back to the index page",
			Files:           bundle,
			Path:            "/workloads/example",
			ExpectedStatus:  http.StatusOK,
			ExpectedBody:    "<html>index</html>",
			ExpectedCaching: "no-cache",
		},
		{
			Name:            "the index page itself gets the fallback's caching",
			Files:           bundle,
			Path:            "/index.html",
			ExpectedStatus:  http.StatusOK,
			ExpectedBody:    "<html>index</html>",
			ExpectedCaching: "no-cache",
		},
		{
			Name:            "hashed assets are cached indefinitely",
			Files:           bundle,
			Path:            "/assets/app.abc.js",
			ExpectedStatus:  http.StatusOK,
			ExpectedBody:    "js",
			ExpectedCaching: "public, max-age=31536000, immutable",
		},
		{
			Name:           "unhashed files are served without caching headers",
			Files:          bundle,
			Path:           "/takt.svg",
			ExpectedStatus: http.StatusOK,
			ExpectedBody:   "svg",
		},
		{
			Name:           "a bundle without an index page reports the ui is absent",
			Files:          fstest.MapFS{},
			Path:           "/",
			ExpectedStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.Path, nil)
			w := httptest.NewRecorder()

			ui.NewHandler(tc.Files).ServeHTTP(w, req)

			assert.Equal(t, tc.ExpectedStatus, w.Code)
			assert.Equal(t, tc.ExpectedCaching, w.Header().Get("Cache-Control"))
			if tc.ExpectedBody != "" {
				assert.Equal(t, tc.ExpectedBody, w.Body.String())
			}
		})
	}
}
