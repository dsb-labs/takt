package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dsb-labs/takt/internal/generated/api"
)

type (
	// The Rekey type is the client-side view of a completed rekey.
	Rekey struct {
		// How many secrets were re-encrypted.
		Secrets int
		// The key the server's secrets are now sealed under.
		KeyID string
		// The key they were sealed under before. The server's keyring keeps it,
		// because it still opens the backups taken before the rekey.
		PreviousKeyID string
	}

	// The BackupOption type is a function that modifies what a backup covers.
	BackupOption func(*backupConfig)

	backupConfig struct {
		includeKeys bool
	}
)

// WithKeys modifies a backup to hold the keyring alongside the database.
//
// Left out unless this is passed, and the default is the one to prefer. A database
// without its keys decrypts nothing, and that separability is what makes a copy of
// it safe to keep somewhere a key would not be. An archive holding both is key
// material: it opens every secret the server holds, and it keeps opening them long
// after the backup was taken.
func WithKeys() BackupOption {
	return func(c *backupConfig) { c.includeKeys = true }
}

// Backup writes a backup of the server's state to out, as a zip archive.
//
// The archive holds a consistent snapshot of the database taken while the server
// keeps running, and the keyring when WithKeys is passed. It holds neither volume
// data, which the server has no business copying, nor the files a workload mounts,
// which are transient. Volumes are backed up separately, and ListVolumes reports
// where each one is on the host.
//
// The archive is copied as it arrives rather than returned, like Metrics: it is the
// size of the server's database, and the usual destination is a file.
func (c *Client) Backup(ctx context.Context, out io.Writer, options ...BackupOption) error {
	var config backupConfig
	for _, option := range options {
		option(&config)
	}

	var params api.GetBackupParams
	if config.includeKeys {
		params.IncludeKeys = new(true)
	}

	// The client with no request timeout. How long a backup takes is the size of the
	// server's database divided by the speed of the link, and the timeout covers
	// reading the body as well as sending the request. What ends this is the caller's
	// context.
	resp, err := c.stream.GetBackup(ctx, &params)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body api.ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body)

		return newError(resp.StatusCode, &body)
	}

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to read the response body: %w", err)
	}

	return nil
}

// Rekey re-encrypts every secret the server holds under a newly generated key.
//
// No workload is redeployed by this. A rekey changes how a value is stored, not what
// it is, so no secret's revision moves.
//
// The keyring needs a fresh backup afterwards. The copy taken before this call no
// longer opens anything the server holds.
func (c *Client) Rekey(ctx context.Context) (Rekey, error) {
	// The client with no request timeout. How long a rekey takes is the number of
	// secrets the server holds, which is the operator's business rather than
	// something a fixed deadline should decide.
	resp, err := c.stream.RekeyWithResponse(ctx, api.RekeyJSONRequestBody{})
	if err != nil {
		return Rekey{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return Rekey{
			Secrets:       resp.JSON200.Secrets,
			KeyID:         resp.JSON200.KeyID,
			PreviousKeyID: resp.JSON200.PreviousKeyID,
		}, nil
	case resp.JSON500 != nil:
		return Rekey{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Rekey{}, newError(resp.StatusCode(), nil)
	}
}
