// Package dev provides the CLI endpoint to the "dev" command, which groups the
// commands used to develop orca rather than to operate it.
package dev

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/cmd/dev/loadtest"
)

// Command returns the "dev" command, which does nothing on its own and holds the
// commands used while working on orca.
//
// Hidden, because these are not commands an operator has any use for. They ship in
// the binary rather than in a second one so that a load test runs against the build
// under test, which is the only build whose numbers mean anything.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dev",
		Short:  "Commands for developing orca",
		Hidden: true,
		Long: "Commands for developing orca.\n\n" +
			"These act on a server the way a developer does rather than the way an\n" +
			"operator does, and they are documented in CONTRIBUTING.md rather than in\n" +
			"the command line reference.",
	}

	cmd.AddCommand(
		loadtest.Command(),
	)

	return cmd
}
