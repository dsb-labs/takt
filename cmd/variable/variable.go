// Package variable provides the CLI endpoint to the "variable" command, which groups
// the commands that act on variables.
package variable

import (
	"github.com/spf13/cobra"

	delcmd "github.com/dsb-labs/orca/cmd/variable/delete"
	"github.com/dsb-labs/orca/cmd/variable/get"
	"github.com/dsb-labs/orca/cmd/variable/list"
	"github.com/dsb-labs/orca/cmd/variable/set"
)

// Command returns the "variable" command, which does nothing on its own and holds the
// commands that act on a variable.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "variable",
		Short: "Set, inspect and delete variables",
		Long: "Set, inspect and delete variables.\n\n" +
			"A variable is a value a workload reads and an operator can. It is stored as\n" +
			"given, referenced from a manifest as ${var:name}, and reported by every\n" +
			"command here. Changing one replaces the workloads reading it.\n\n" +
			"Use a variable for a hostname, a log level, or anything else you would want\n" +
			"to read back later. Use a secret for anything that would be damaging to\n" +
			"report, since these values are returned to whoever can reach the server.",
	}

	cmd.AddCommand(
		set.Command(),
		list.Command(),
		get.Command(),
		delcmd.Command(),
	)

	return cmd
}
