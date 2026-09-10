// Package create provides the CLI endpoint to the "token create" command.
package create

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// The Result type is what the command prints: the credential and the record
// of it, together, because the credential appears exactly once.
type Result struct {
	// The token itself. Store it now: the server keeps only a hash, so it
	// cannot be shown again.
	Credential string
	// The record of the token, which is what "token list" reports from now
	// on.
	Token client.Token
}

// Command returns the "token create" command used to mint a token for a
// principal.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "create <principal>",
		Short: "Create a token for a principal",
		Long: "Create a token for a principal.\n\n" +
			"The principal is the name the policy grants roles to. By convention humans\n" +
			"are emails and machines are bare names, so an identity provider's username\n" +
			"cannot collide with a machine's grants.\n\n" +
			"The credential is printed once and never stored. The principal need not be\n" +
			"granted anything yet: merge the grant, then hand over the token, in\n" +
			"whichever order onboarding runs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			token, credential, err := c.CreateToken(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to create token: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(Result{Credential: credential, Token: token})
		},
	}
}
