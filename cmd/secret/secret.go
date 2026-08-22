// Package secret provides the CLI endpoint to the "secret" command, which groups the
// commands that act on secrets.
package secret

import (
	"github.com/spf13/cobra"

	delcmd "github.com/dsb-labs/orca/cmd/secret/delete"
	"github.com/dsb-labs/orca/cmd/secret/get"
	"github.com/dsb-labs/orca/cmd/secret/list"
	"github.com/dsb-labs/orca/cmd/secret/set"
)

// Command returns the "secret" command, which does nothing on its own and holds the
// commands that act on a secret.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Set, inspect and delete secrets",
		Long: "Set, inspect and delete secrets.\n\n" +
			"A secret is a value a workload reads and an operator cannot. It is stored\n" +
			"encrypted, referenced from a manifest as ${secret:name}, and decrypted only\n" +
			"to be handed to a workload as it starts. Nothing reads one back out: there\n" +
			"is no command here that prints a value, because there is no endpoint that\n" +
			"returns one.",
	}

	cmd.AddCommand(
		set.Command(),
		list.Command(),
		get.Command(),
		delcmd.Command(),
	)

	return cmd
}
