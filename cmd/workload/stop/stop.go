// Package stop provides the CLI endpoint to the "workload stop" command.
package stop

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "workload stop" command used to suspend a workload and hold
// it down.
func Command() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a workload and hold it down",
		Long: "Stop a workload and hold it down.\n\n" +
			"Stopping is asynchronous: the workload is marked as suspended and the server\n" +
			"stops its instances afterwards. Suspension survives a server restart and holds\n" +
			"until \"workload start\" clears it. The specification and its version are\n" +
			"untouched, so starting the workload resumes it rather than replacing it.\n" +
			"Pass --wait to block until nothing is running for it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.LifecycleOption
			if wait {
				options = append(options, client.WithWait())
			}

			if _, err := c.Stop(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to stop workload: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "block until nothing is running for the workload")

	return cmd
}
