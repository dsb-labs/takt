// Package delete provides the CLI endpoint to the "token delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "token delete" command used to revoke a token.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Revoke a token",
		Long: "Revoke a token by the identifier \"token list\" reports.\n\n" +
			"Revocation is immediate: the next request presenting the credential is\n" +
			"refused. The recovery token can be revoked here too, which is safe as\n" +
			"long as an admin credential remains — and recoverable through the reset\n" +
			"file if not.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			if err := c.DeleteToken(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("failed to delete token: %w", err)
			}

			return nil
		},
	}
}
