// Package get provides the CLI endpoint to the "workload get" command.
package get

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "workload get" command used to show a single workload.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a single workload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			workload, err := c.Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get workload: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(workload)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
