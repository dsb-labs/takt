package client_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/client"
)

func TestClient_Backup(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Options      []client.BackupOption
		Handler      http.HandlerFunc
		Expect       string
		ExpectsError bool
	}{
		{
			Name: "writes the archive",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/admin/backup", r.URL.Path)

				// Absent rather than false. The server's own default is what
				// decides, so a client that says nothing asks for nothing.
				assert.Empty(t, r.URL.Query().Get("includeKey"))

				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write([]byte("archive"))
			},
			Expect: "archive",
		},
		{
			Name:    "asks for the key",
			Options: []client.BackupOption{client.WithKey()},
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "true", r.URL.Query().Get("includeKey"))

				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write([]byte("archive with key"))
			},
			Expect: "archive with key",
		},
		{
			Name: "server failure",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "failed to prepare backup"})
			},
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			server := httptest.NewServer(tc.Handler)
			t.Cleanup(server.Close)

			c, err := client.New(server.URL)
			require.NoError(t, err)

			var out bytes.Buffer

			err = c.Backup(t.Context(), &out, tc.Options...)
			if tc.ExpectsError {
				assert.Error(t, err)

				// Nothing written, so a caller that failed does not leave a partial
				// file looking like a backup.
				assert.Empty(t, out.String())

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expect, out.String())
		})
	}
}
