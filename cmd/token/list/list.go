// Package list provides the CLI endpoint to the "token list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "token list" command used to name every credential the
// server holds.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List every credential the server holds",
		Long: "List every credential the server holds.\n\n" +
			"Static tokens, logins, sessions and the recovery token all appear, with\n" +
			"when each was created and last used. No credential itself is reported,\n" +
			"only the records of them. With \"takt acl get\", this answers who can\n" +
			"touch this server, completely.",
		Args: cobra.NoArgs,
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
