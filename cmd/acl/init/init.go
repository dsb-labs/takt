// Package init provides the CLI endpoint to the "acl init" command.
package init

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// The Result type is what the command prints: the recovery token, which
// appears exactly once.
type Result struct {
	// The recovery token. Store it now: the server keeps only a hash, so it
	// cannot be shown again, and losing it means the reset file.
	Credential string
}

//go:embed usage.txt
var usage string

// Command returns the "acl init" command used to mint the recovery token.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Mint the recovery token",
		Long:  usage,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			credential, err := c.InitACL(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to initialize acl: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(Result{Credential: credential})
		},
	}
}
