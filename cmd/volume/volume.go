// Package volume provides the CLI endpoint to the "volume" command, which groups the
// commands that act on volumes.
package volume

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/cmd/volume/create"
	delcmd "github.com/dsb-labs/orca/cmd/volume/delete"
	"github.com/dsb-labs/orca/cmd/volume/get"
	"github.com/dsb-labs/orca/cmd/volume/list"
	"github.com/dsb-labs/orca/cmd/volume/update"
)

// Command returns the "volume" command, which does nothing on its own and holds the
// commands that act on a volume.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Create, inspect and delete volumes",
		Long: "Create, inspect and delete volumes.\n\n" +
			"A volume is storage a workload mounts, with a lifetime of its own. It is\n" +
			"created before the workload that mounts it and outlives that workload, so\n" +
			"deleting a workload never destroys what it stored.",
	}

	cmd.AddCommand(
		create.Command(),
		list.Command(),
		update.Command(),
		get.Command(),
		delcmd.Command(),
	)

	return cmd
}
