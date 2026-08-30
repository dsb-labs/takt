package client_test

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/generated/api"
	"github.com/dsb-labs/orca/pkg/client"
)

func TestNew(t *testing.T) {
	t.Parallel()

	healthy := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GetHealthResult{Status: api.Ok})
	}

	t.Run("trusts a server through a ca certificate", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(healthy))
		t.Cleanup(server.Close)

		c, err := client.New(server.URL, client.WithCACertificate(writeCertificate(t, server)))
		require.NoError(t, err)

		assert.NoError(t, c.Health(t.Context()))
	})

	t.Run("refuses a server it cannot verify", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(healthy))
		t.Cleanup(server.Close)

		c, err := client.New(server.URL)
		require.NoError(t, err)

		assert.Error(t, c.Health(t.Context()))
	})

	t.Run("refuses a missing ca certificate file", func(t *testing.T) {
		_, err := client.New("http://localhost:7373", client.WithCACertificate(filepath.Join(t.TempDir(), "missing.pem")))
		assert.Error(t, err)
	})

	t.Run("refuses a ca certificate file with no certificates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-a-cert.pem")
		require.NoError(t, os.WriteFile(path, []byte("not pem"), 0o600))

		_, err := client.New("http://localhost:7373", client.WithCACertificate(path))
		assert.Error(t, err)
	})
}

// writeCertificate writes the test server's certificate to a file in PEM form
// and returns its path.
func writeCertificate(t *testing.T, server *httptest.Server) string {
	t.Helper()

	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})

	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))

	return path
}
