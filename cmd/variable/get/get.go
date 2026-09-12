// Package get provides the CLI endpoint to the "variable get" command.
package get

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "variable get" command used to read a single variable.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a single variable",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			variable, err := c.GetVariable(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get variable: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(variable)
		},
	}

	return cmd
}
