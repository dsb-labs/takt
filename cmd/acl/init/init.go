// Package init provides the CLI endpoint to the "acl init" command.
package init

import (
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

// Command returns the "acl init" command used to mint the recovery token.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Mint the recovery token",
		Long: "Mint the recovery token, exactly once.\n\n" +
			"The recovery token sits above policy and exists for init and lockout\n" +
			"recovery, not for daily use. Apply the first policy with it, or create\n" +
			"the first admin token, and then put it somewhere safe.\n\n" +
			"A second init is refused for as long as a recovery token exists. Losing\n" +
			"the token is recovered at the host: write a file named acl.reset into\n" +
			"the server's data directory, restart the server, and init works again.",
		Args: cobra.NoArgs,
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
