package api_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/api"
	"github.com/dsb-labs/orca/internal/server/database"
	secretstore "github.com/dsb-labs/orca/internal/server/secret"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestAdminAPI_GetBackup(t *testing.T) {
	t.Parallel()

	// The real service rather than a mock, because a prepared backup is only
	// produced by taking one. What this asserts is that the endpoint hands the
	// archive over intact, which a stand-in could not show.
	t.Run("streams an archive holding the database", func(t *testing.T) {
		admin, _ := newAdminService(t)

		resp := doAdmin(t, admin, "/api/v1/admin/backup")

		require.Equal(t, http.StatusOK, resp.Code)
		assert.Equal(t, "application/zip", resp.Header().Get("Content-Type"))
		assert.Equal(t, []string{"state.db"}, archivedNames(t, resp.Body.Bytes()))
	})

	t.Run("includes the keyring when asked", func(t *testing.T) {
		admin, keyID := newAdminService(t)

		resp := doAdmin(t, admin, "/api/v1/admin/backup?includeKeys=true")

		require.Equal(t, http.StatusOK, resp.Code)

		names := archivedNames(t, resp.Body.Bytes())
		slices.Sort(names)
		assert.Equal(t, []string{"keys/" + keyID + ".key", "state.db"}, names)
	})

	// The reason preparing and streaming are separate calls. A snapshot that could
	// not be taken has to come back as a failure, not as an archive that stops early
	// and looks like it worked.
	t.Run("reports a snapshot that could not be taken", func(t *testing.T) {
		admin := NewMockAdmin(t)
		admin.EXPECT().
			PrepareBackup(mock.Anything, service.BackupOptions{}).
			Return(nil, errors.New("no space left on device")).
			Once()

		resp := doAdmin(t, admin, "/api/v1/admin/backup")

		require.Equal(t, http.StatusInternalServerError, resp.Code)
		assert.JSONEq(t, `{"error":"failed to prepare backup"}`, resp.Body.String())
	})
}

// newAdminService returns a service over a data directory holding a database and a
// keyring, so that there is something to back up, along with the key's identifier.
func newAdminService(t *testing.T) (*service.AdminService, string) {
	t.Helper()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	db, err := database.Open(t.Context(), database.Config{
		Logger: logger,
		Path:   filepath.Join(dir, "state.db"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	keys, err := secretstore.NewStore(filepath.Join(dir, "keys"))
	require.NoError(t, err)

	id, err := keys.Create()
	require.NoError(t, err)

	return service.NewAdminService(service.AdminServiceConfig{
		Logger:   logger,
		Database: filepath.Join(dir, "state.db"),
		Keys:     keys,
	}), id
}

func archivedNames(t *testing.T, archive []byte) []string {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)

	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
	}

	return names
}

func doAdmin(t *testing.T, admin api.Admin, target string) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError}))

	// The whole surface is registered even for a test about one resource, since the
	// generated router serves one API and a request for an unregistered route would
	// come back as a routing failure rather than as the handler's answer.
	mux := http.NewServeMux()
	api.New(api.Config{
		Workloads: api.NewWorkloadAPI(api.WorkloadAPIConfig{Logger: logger, Workloads: NewMockWorkloadService(t)}),
		Volumes:   api.NewVolumeAPI(api.VolumeAPIConfig{Logger: logger, Volumes: NewMockVolumeService(t)}),
		Secrets:   api.NewSecretAPI(api.SecretAPIConfig{Logger: logger, Secrets: NewMockSecretService(t)}),
		Variables: api.NewVariableAPI(api.VariableAPIConfig{Logger: logger, Variables: NewMockVariableService(t)}),
		System: api.NewSystemAPI(api.SystemAPIConfig{
			Logger:   logger,
			DB:       NewMockPinger(t),
			Observer: NewMockObserver(t),
		}),
		Admin: api.NewAdminAPI(api.AdminAPIConfig{Logger: logger, Admin: admin}),
	}).Register(mux)

	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	return resp
}
