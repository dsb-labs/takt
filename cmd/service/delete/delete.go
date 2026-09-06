// Package delete provides the CLI endpoint to the "service delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "service delete" command used to remove a service.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a service",
		Long: "Delete a service.\n\n" +
			"The workloads the service selected keep running. What stops is the\n" +
			"service reporting their addresses.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			if err := c.DeleteService(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("failed to delete service: %w", err)
			}

			return nil
		},
	}

	return cmd
}
