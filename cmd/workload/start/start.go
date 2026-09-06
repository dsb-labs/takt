// Package start provides the CLI endpoint to the "workload start" command.
package start

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "workload start" command used to resume a stopped workload.
func Command() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a stopped workload",
		Long: "Start a stopped workload.\n\n" +
			"Starting is asynchronous: the suspension is cleared and the server starts the\n" +
			"workload's instances on its next pass, from whatever specification is stored.\n" +
			"A scheduled workload waits for its next occurrence rather than running the\n" +
			"ones it missed. Starting a workload that is not stopped changes nothing.\n" +
			"Pass --wait to block until the workload has left pending.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.LifecycleOption
			if wait {
				options = append(options, client.WithWait())
			}

			if _, err := c.Start(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to start workload: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "block until the workload has left pending")

	return cmd
}
