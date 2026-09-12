// Package stop provides the CLI endpoint to the "workload stop" command.
package stop

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload stop" command used to suspend a workload and hold
// it down.
func Command() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a workload and hold it down",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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
