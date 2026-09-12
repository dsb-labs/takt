// Package get provides the CLI endpoint to the "secret get" command.
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

// Command returns the "secret get" command used to read a single secret.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a single secret",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			secret, err := c.GetSecret(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get secret: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(secret)
		},
	}

	return cmd
}
