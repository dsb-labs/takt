// Package admin provides the CLI endpoint to the "admin" command, which groups the
// commands that act on the node itself.
package admin

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/cmd/admin/backup"
	"github.com/dsb-labs/orca/cmd/admin/rekey"
	"github.com/dsb-labs/orca/cmd/admin/restore"
)

// Command returns the "admin" command, which does nothing on its own and holds the
// commands that act on the node rather than on the workloads running on it.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operate on the node itself",
		Long: "Operate on the node itself.\n\n" +
			"These commands act on the state orca keeps rather than on the workloads\n" +
			"it runs. What they touch is shared by every workload on the node, so a\n" +
			"maintenance window is usually the right time for them.",
	}

	cmd.AddCommand(
		backup.Command(),
		rekey.Command(),
		restore.Command(),
	)

	return cmd
}
