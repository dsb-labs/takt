package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/internal/server/service"
)

type (
	// The Admin interface describes the operations the admin endpoints run against
	// the node itself.
	Admin interface {
		// PrepareBackup should take a snapshot of the state a backup covers. The
		// returned backup should be closed by the caller.
		PrepareBackup(ctx context.Context, options service.BackupOptions) (*service.Backup, error)
		// Rekey should re-encrypt every secret under a newly generated key.
		Rekey(ctx context.Context) (service.Rekey, error)
	}

	// The AdminAPI type exposes the HTTP endpoints acting on the node itself
	// rather than on the workloads running on it.
	AdminAPI struct {
		logger *slog.Logger
		admin  Admin
	}

	// The AdminAPIConfig type contains fields used to construct an AdminAPI.
	AdminAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service performing the operations the endpoints expose.
		Admin Admin
	}
)

// NewAdminAPI returns a new instance of the AdminAPI type.
func NewAdminAPI(config AdminAPIConfig) *AdminAPI {
	return &AdminAPI{
		logger: config.Logger.With("component", "api"),
		admin:  config.Admin,
	}
}

// internalError logs why a request failed and returns the message the client is told
// instead.
func (a *AdminAPI) internalError(operation string, err error) string {
	a.logger.With("error", err, "operation", operation).Error("failed to serve request")

	return "failed to " + operation
}

// GetBackup returns a zip archive holding a consistent snapshot of the database, and
// the keyring when the request asks for it.
func (a *AdminAPI) GetBackup(ctx context.Context, request api.GetBackupRequestObject) (api.GetBackupResponseObject, error) {
	var options service.BackupOptions
	if request.Params.IncludeKeys != nil {
		options.IncludeKeys = *request.Params.IncludeKeys
	}

	// The snapshot is taken before the response starts, so that the failures worth
	// reporting — no database, no room for a copy of it, a key that is not where it
	// was configured — come back as a 500 rather than as an archive that stops
	// early. A truncated zip is a backup that looks like it worked.
	backup, err := a.admin.PrepareBackup(ctx, options)
	if err != nil {
		return api.GetBackup500JSONResponse{
			Error: a.internalError("prepare backup", err),
		}, nil
	}

	return backupResponse{
		logger: a.logger,
		backup: backup,
		// The connection's own writer, which is the only one that can be given a
		// deadline. Nil when nothing put it there, and the transfer then lives
		// under whatever deadline the server set for every request.
		conn: middleware.Connection(ctx),
	}, nil
}

// The backupResponse type streams a backup archive to the client.
//
// The generated response type for this endpoint takes a reader, which would mean
// assembling the whole archive somewhere before sending any of it. This writes
// straight to the response instead, so the server's memory use doesn't scale with
// the size of the database.
type backupResponse struct {
	logger *slog.Logger
	backup *service.Backup
	// The connection's own writer, for the deadline the wrappers cannot carry.
	conn http.ResponseWriter
}

// VisitGetBackupResponse writes the backup to w as a zip archive.
func (r backupResponse) VisitGetBackupResponse(w http.ResponseWriter) error {
	defer func() {
		if err := r.backup.Close(); err != nil {
			r.logger.With("error", err).Warn("failed to remove backup snapshot")
		}
	}()

	// How long the transfer takes is the size of the operator's database divided by
	// the speed of their link, and no fixed deadline knows either. One that expired
	// would end the response as a truncated archive at exactly the timeout, which
	// reads as a backup that completed.
	//
	// Nothing is left unbounded by this. The request's context ends the transfer
	// when the caller disconnects, which is what actually limits how long this
	// occupies the server.
	//
	// A writer with no deadline to clear says so, and there is nothing to do about
	// that but carry on.
	deadline := r.conn
	if deadline == nil {
		deadline = w
	}

	err := http.NewResponseController(deadline).SetWriteDeadline(time.Time{})
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("failed to clear the write deadline: %w", err)
	}

	// No Content-Length. Knowing it would mean compressing the archive twice, or
	// holding it in memory, to save a client a progress bar.
	w.Header().Set("Content-Type", "application/zip")
	w.WriteHeader(http.StatusOK)

	return r.backup.Stream(w)
}

// Rekey re-encrypts every secret under a newly generated key and reports what moved.
func (a *AdminAPI) Rekey(ctx context.Context, _ api.RekeyRequestObject) (api.RekeyResponseObject, error) {
	rekey, err := a.admin.Rekey(ctx)
	if err != nil {
		return api.Rekey500JSONResponse{
			Error: a.internalError("rekey the node", err),
		}, nil
	}

	return api.Rekey200JSONResponse{
		Secrets:       rekey.Secrets,
		KeyID:         rekey.KeyID,
		PreviousKeyID: rekey.PreviousKeyID,
	}, nil
}
