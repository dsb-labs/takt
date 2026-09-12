// Package token provides the CLI endpoint to the "token" command, which groups
// the commands that act on tokens.
package token

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/token/create"
	delcmd "github.com/dsb-labs/takt/cmd/token/delete"
	"github.com/dsb-labs/takt/cmd/token/list"
)

//go:embed usage.txt
var usage string

// Command returns the "token" command, which does nothing on its own and holds
// the commands that act on a token.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke tokens",
		Long:  usage,
	}

	cmd.AddCommand(
		create.Command(),
		list.Command(),
		delcmd.Command(),
	)

	return cmd
}
