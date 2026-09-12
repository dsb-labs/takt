// Package restart provides the CLI endpoint to the "workload restart" command.
package restart

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload restart" command used to replace a workload's
// running instances.
func Command() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "restart <name>",
		Short: "Replace a workload's running instances",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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
