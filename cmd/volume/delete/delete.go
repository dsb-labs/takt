// Package delete provides the CLI endpoint to the "volume delete" command.
package delete

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "volume delete" command used to remove a volume and everything
// stored in it.
func Command() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a volume and the data it holds",
		Long:    usage,
		Args:    cobra.ExactArgs(1),
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
