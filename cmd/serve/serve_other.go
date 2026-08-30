//go:build !linux

// Package serve provides the CLI endpoint to the "serve" command.
package serve

import (
	"errors"

	"github.com/spf13/cobra"
)

// Command returns a hidden stub of the "serve" command. The server only runs
// on linux, so on other platforms the command refuses and stays out of help.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:    "serve [config-file]",
		Short:  "Run the orca server",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("the orca server only runs on linux")
		},
	}
}
