// Package create provides the CLI endpoint to the "token create" command.
package create

import (
	_ "embed"
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

//go:embed usage.txt
var usage string

// Command returns the "token create" command used to mint a token for a
// principal.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "create <principal>",
		Short: "Create a token for a principal",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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
