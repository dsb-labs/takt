//go:build !linux

// Package restore provides the CLI endpoint to the "admin restore" command.
package restore

import (
	"errors"

	"github.com/spf13/cobra"
)

// Command returns a hidden stub of the "admin restore" command. A restore works
// over the server's data directory, and the server only runs on linux, so on
// other platforms the command refuses and stays out of help.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:    "restore <archive> [config-file]",
		Short:  "Put a node back from a backup archive",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("the orca server only runs on linux")
		},
	}
}
