// Package get provides the CLI endpoint to the "service get" command.
package get

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "service get" command used to show a single service.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a single service",
		Long: "Show a single service.\n\n" +
			"Reports the service's target and the backends it selects: the address of\n" +
			"every selected instance that is running, passing its check when the\n" +
			"workload declares one, and not being torn down.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			got, err := c.GetService(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get service: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(got)
		},
	}

	return cmd
}
