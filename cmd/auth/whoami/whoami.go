// Package whoami provides the CLI endpoint to the "auth whoami" command.
package whoami

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "auth whoami" command used to report the caller's own
// identity.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Report who the server thinks you are",
		Long: "Report the principal, role and groups the server resolves your credential\n" +
			"to. It needs authentication but no role, so a principal the policy grants\n" +
			"nothing yet sees exactly that state — which is what onboarding looks like\n" +
			"from the inside.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			identity, err := c.WhoAmI(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to read identity: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(identity)
		},
	}
}
