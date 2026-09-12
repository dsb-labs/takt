// Package variable provides the CLI endpoint to the "variable" command, which groups
// the commands that act on variables.
package variable

import (
	_ "embed"
	"github.com/spf13/cobra"

	delcmd "github.com/dsb-labs/takt/cmd/variable/delete"
	"github.com/dsb-labs/takt/cmd/variable/get"
	"github.com/dsb-labs/takt/cmd/variable/list"
	"github.com/dsb-labs/takt/cmd/variable/set"
)

//go:embed usage.txt
var usage string

// Command returns the "variable" command, which does nothing on its own and holds the
// commands that act on a variable.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "variable",
		Short: "Set, inspect and delete variables",
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
