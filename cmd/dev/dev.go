// Package dev provides the CLI endpoint to the "dev" command, which groups the
// commands used to develop takt rather than to operate it.
package dev

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/dev/loadtest"
)

//go:embed usage.txt
var usage string

// Command returns the "dev" command, which does nothing on its own and holds the
// commands used while working on takt.
//
// Hidden, because these are not commands an operator has any use for. They ship in
// the binary rather than in a second one so that a load test runs against the build
// under test, which is the only build whose numbers mean anything.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dev",
		Short:  "Commands for developing takt",
		Hidden: true,
		Long:   usage,
	}

	cmd.AddCommand(
		loadtest.Command(),
	)

	return cmd
}
