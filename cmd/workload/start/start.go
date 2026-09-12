// Package start provides the CLI endpoint to the "workload start" command.
package start

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload start" command used to resume a stopped workload.
func Command() *cobra.Command {
	var wait bool

	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a stopped workload",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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
