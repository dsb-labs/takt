// Package delete provides the CLI endpoint to the "volume delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "volume delete" command used to remove a volume and everything
// stored in it.
func Command() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a volume and the data it holds",
		Long: "Delete a volume and the data it holds.\n\n" +
			"This is the only thing in takt that destroys stored data. A volume a\n" +
			"workload mounts is refused, and the workloads holding it are named; pass\n" +
			"--force to remove it anyway, which leaves those workloads running with a\n" +
			"mount that no longer resolves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.DeleteVolumeOption
			if force {
				options = append(options, client.WithForce())
			}

			if err := c.DeleteVolume(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to delete volume: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove the volume even though a workload mounts it")

	return cmd
}
