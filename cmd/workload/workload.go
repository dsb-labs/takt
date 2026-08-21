// Package workload provides the CLI endpoint to the "workload" command, which groups
// the commands that act on workloads.
package workload

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/cmd/workload/apply"
	delcmd "github.com/dsb-labs/orca/cmd/workload/delete"
	"github.com/dsb-labs/orca/cmd/workload/get"
	"github.com/dsb-labs/orca/cmd/workload/list"
	"github.com/dsb-labs/orca/cmd/workload/logs"
)

// Command returns the "workload" command, which does nothing on its own and holds the
// commands that act on a workload.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workload",
		Short: "Apply, inspect and delete workloads",
	}

	cmd.AddCommand(
		apply.Command(),
		list.Command(),
		get.Command(),
		delcmd.Command(),
		logs.Command(),
	)

	return cmd
}
