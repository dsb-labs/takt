// Package delete provides the CLI endpoint to the "workload delete" command.
package delete

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload delete" command used to remove a workload and stop
// everything running for it.
func Command() *cobra.Command {
	var wait bool
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a workload and stop its work",
		Long:    usage,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.LifecycleOption
			if wait {
				options = append(options, client.WithWait())
			}
			if force {
				options = append(options, client.WithForceDeleteWorkload())
			}

			if _, err := c.Delete(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to delete workload: %w", err)
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.BoolVarP(&wait, "wait", "w", false, "block until the workload has finished terminating")
	flags.BoolVarP(&force, "force", "f", false, "delete the workload even though another references it")

	return cmd
}
