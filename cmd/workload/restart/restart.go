// Package restart provides the CLI endpoint to the "workload restart" command.
package restart

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "workload restart" command used to replace a workload's
// running instances.
func Command() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "restart <name>",
		Short: "Replace a workload's running instances",
		Long: "Replace a workload's running instances.\n\n" +
			"The restart happens on the server's next pass, from the unchanged\n" +
			"specification, so the version does not move. A stopped workload is refused,\n" +
			"since nothing would start until it is started again. Pass --wait to block\n" +
			"until a replacement instance has appeared.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.LifecycleOption
			if wait {
				options = append(options, client.WithWait())
			}

			if _, err := c.Restart(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to restart workload: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "block until a replacement instance has appeared")

	return cmd
}
