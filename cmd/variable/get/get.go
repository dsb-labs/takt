// Package get provides the CLI endpoint to the "variable get" command.
package get

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "variable get" command used to read a single variable.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a single variable",
		Long: "Get a single variable, with its value and the workloads reading it.\n\n" +
			"The value is part of the output, where a secret's is not. Being able to\n" +
			"confirm what a workload is configured with is the reason to choose a\n" +
			"variable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			variable, err := c.GetVariable(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get variable: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(variable)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
