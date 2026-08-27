package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dsb-labs/orca/internal/generated/api"
)

type (
	// The BackupOption type is a function that modifies what a backup covers.
	BackupOption func(*backupConfig)

	backupConfig struct {
		includeKey bool
	}
)

// WithKey modifies a backup to hold the secret encryption key alongside the
// database.
//
// Left out unless this is passed, and the default is the one to prefer. A database
// without its key decrypts nothing, and that separability is what makes a copy of it
// safe to keep somewhere a key would not be. An archive holding both is key
// material: it opens every secret the server holds, and it keeps opening them long
// after the backup was taken.
func WithKey() BackupOption {
	return func(c *backupConfig) { c.includeKey = true }
}

// Backup writes a backup of the server's state to out, as a zip archive.
//
// The archive holds a consistent snapshot of the database taken while the server
// keeps running. It holds neither volume data, which the server has no business
// copying, nor the files a workload mounts, which are transient. Volumes are backed
// up separately, and ListVolumes reports where each one is on the host.
//
// The archive is copied as it arrives rather than returned, like Metrics: it is the
// size of the server's database, and the usual destination is a file.
func (c *Client) Backup(ctx context.Context, out io.Writer, options ...BackupOption) error {
	var config backupConfig
	for _, option := range options {
		option(&config)
	}

	var params api.GetBackupParams
	if config.includeKey {
		params.IncludeKey = new(true)
	}

	// The client with no request timeout. How long a backup takes is the size of the
	// server's database divided by the speed of the link, and the timeout covers
	// reading the body as well as sending the request. What ends this is the caller's
	// context.
	resp, err := c.stream.GetBackup(ctx, &params)
	if err != nil {
		return fmt.Errorf("failed to read backup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body)

		return newError(resp.StatusCode, &body)
	}

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to read backup: %w", err)
	}

	return nil
}
