// Package list provides the CLI endpoint to the "token list" command.
package list

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "token list" command used to name every credential the
// server holds.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List every credential the server holds",
		Long:    usage,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			tokens, err := c.ListTokens(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list tokens: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(tokens)
		},
	}
}
