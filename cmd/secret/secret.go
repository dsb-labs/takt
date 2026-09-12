// Package secret provides the CLI endpoint to the "secret" command, which groups the
// commands that act on secrets.
package secret

import (
	_ "embed"
	"github.com/spf13/cobra"

	delcmd "github.com/dsb-labs/takt/cmd/secret/delete"
	"github.com/dsb-labs/takt/cmd/secret/get"
	"github.com/dsb-labs/takt/cmd/secret/list"
	"github.com/dsb-labs/takt/cmd/secret/set"
)

//go:embed usage.txt
var usage string

// Command returns the "secret" command, which does nothing on its own and holds the
// commands that act on a secret.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Set, inspect and delete secrets",
		Long:  usage,
	}

	cmd.AddCommand(
		set.Command(),
		list.Command(),
		get.Command(),
		delcmd.Command(),
	)

	return cmd
}
