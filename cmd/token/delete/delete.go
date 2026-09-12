// Package delete provides the CLI endpoint to the "token delete" command.
package delete

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "token delete" command used to revoke a token.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Revoke a token",
		Long:    usage,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			if err := c.DeleteToken(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("failed to delete token: %w", err)
			}

			return nil
		},
	}
}
