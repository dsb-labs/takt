//go:build linux

// Package restore provides the CLI endpoint to the "admin restore" command.
package restore

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/internal/restore"
	"github.com/dsb-labs/takt/internal/server"
)

// How long the check for a running server waits for a connection. Long enough for a
// loopback address to answer and short enough that a restore is not held up by one.
const dialTimeout = 2 * time.Second

//go:embed usage.txt
var usage string

// Command returns the "admin restore" command used to put a node back from a backup
// archive.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore <archive> [config-file]",
		Short: "Put a node back from a backup archive",
		Long:  usage,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			config := server.DefaultConfig()

			if len(args) > 1 {
				var err error
				config, err = server.LoadConfig(args[1])
				if err != nil {
					return fmt.Errorf("failed to load configuration file: %w", err)
				}
			}

			// Validated for the data directory it resolves, which is what everything
			// below is derived from. A relative one would restore into whichever
			// directory the command was run from.
			if err := config.Validate(); err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}

			if err := stopped(config.HTTP.Address); err != nil {
				return err
			}

			report, err := restore.Run(cmd.Context(), restore.Config{
				Archive:  args[0],
				Database: config.DatabasePath(),
				Keys:     config.KeysPath(),
				Volumes:  config.VolumesPath(),
			})
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(report)
		},
	}

	return cmd
}

// stopped reports whether the node is stopped, by asking whether anything holds the
// address the server would be listening on.
//
// This is the check a written procedure cannot make, and the reason for restoring
// through a command at all. A restore under a running server writes a database out
// from under the connections reading it, and what the server then does with the
// pages it has cached is not worth finding out.
//
// A connection is the question rather than a health check. What matters is whether
// something holds the port, which does not depend on the address having a scheme or
// on that something answering like takt.
func stopped(address string) error {
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return nil
	}
	defer conn.Close()

	return fmt.Errorf("something is listening on %s: stop the server before restoring over its data directory", address)
}
