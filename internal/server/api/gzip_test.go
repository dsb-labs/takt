package api_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/api"
)

func TestGzip(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("a body worth compressing. ", 100)

	tt := []struct {
		Name           string
		AcceptEncoding string
		Range          string
		Handler        http.HandlerFunc
		ExpectEncoding string
		ExpectBody     string
	}{
		{
			Name:           "compresses for a client that accepts gzip",
			AcceptEncoding: "gzip",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			},
			ExpectEncoding: "gzip",
			ExpectBody:     body,
		},
		{
			Name: "leaves a client that does not accept gzip alone",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			},
			ExpectBody: body,
		},
		{
			Name:           "leaves a range request alone",
			AcceptEncoding: "gzip",
			Range:          "bytes=0-10",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			},
			ExpectBody: body,
		},
		{
			Name:           "leaves a response without a body alone",
			AcceptEncoding: "gzip",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
		},
		{
			Name:           "leaves a response the handler already encoded alone",
			AcceptEncoding: "gzip",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "identity")
				_, _ = w.Write([]byte(body))
			},
			ExpectEncoding: "identity",
			ExpectBody:     body,
		},
		{
			Name:           "drops the uncompressed content length",
			AcceptEncoding: "gzip",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "2600")
				_, _ = w.Write([]byte(body))
			},
			ExpectEncoding: "gzip",
			ExpectBody:     body,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.AcceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tc.AcceptEncoding)
			}
			if tc.Range != "" {
				req.Header.Set("Range", tc.Range)
			}

			w := httptest.NewRecorder()
			api.Gzip(tc.Handler).ServeHTTP(w, req)

			assert.Equal(t, tc.ExpectEncoding, w.Header().Get("Content-Encoding"))

			if tc.ExpectBody == "" {
				return
			}

			if tc.ExpectEncoding == "gzip" {
				reader, err := gzip.NewReader(w.Body)
				require.NoError(t, err)

				decoded, err := io.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, tc.ExpectBody, string(decoded))

				return
			}

			assert.Equal(t, tc.ExpectBody, w.Body.String())
		})
	}
}

// A followed log read flushes mid-response, so a flush has to push what the
// compressor holds far enough for the client to decode it before the stream
// ends.
func TestGzip_Flush(t *testing.T) {
	t.Parallel()

	handler := api.Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first line\n"))
		require.NoError(t, http.NewResponseController(w).Flush())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.True(t, w.Flushed)

	reader, err := gzip.NewReader(w.Body)
	require.NoError(t, err)

	decoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "first line\n", string(decoded))
}
