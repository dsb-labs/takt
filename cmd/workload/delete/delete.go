// Package delete provides the CLI endpoint to the "workload delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "workload delete" command used to remove a workload and stop
// everything running for it.
func Command() *cobra.Command {
	var address string
	var wait bool
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a workload and stop its work",
		Long: "Delete a workload and stop its work.\n\n" +
			"Deletion is asynchronous: the workload is reported as terminating while its\n" +
			"instances are stopped, and disappears once nothing is left running for it.\n" +
			"Pass --wait to block until the teardown has finished.\n\n" +
			"A workload another one references is refused, and the error names the workloads\n" +
			"reading its address. Pass --force to delete it anyway. Those workloads are then\n" +
			"redeployed and report the reference they can no longer resolve.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			var options []client.LifecycleOption
			if wait {
				options = append(options, client.WithWait())
			}
			if force {
				options = append(options, client.WithForceDeleteWorkload())
			}

			if _, err = c.Delete(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to delete workload: %w", err)
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.BoolVarP(&wait, "wait", "w", false, "block until the workload has finished terminating")
	flags.BoolVarP(&force, "force", "f", false, "delete the workload even though another references it")

	return cmd
}
