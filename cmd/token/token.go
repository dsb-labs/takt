// Package token provides the CLI endpoint to the "token" command, which groups
// the commands that act on tokens.
package token

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/token/create"
	delcmd "github.com/dsb-labs/takt/cmd/token/delete"
	"github.com/dsb-labs/takt/cmd/token/list"
)

// Command returns the "token" command, which does nothing on its own and holds
// the commands that act on a token.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke tokens",
		Long: "Create, list and revoke tokens.\n\n" +
			"A token is the credential an API caller presents. It binds to a principal\n" +
			"name, and the policy applied with \"takt acl apply\" decides what that\n" +
			"principal may do. The server stores only a hash, so a created token is\n" +
			"printed once and never again.\n\n" +
			"These commands require the admin role.",
	}

	cmd.AddCommand(
		create.Command(),
		list.Command(),
		delcmd.Command(),
	)

	return cmd
}
