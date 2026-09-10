// Package get provides the CLI endpoint to the "acl get" command.
package get

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "acl get" command used to read the policy document.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Get the policy document",
		Long: "Get the canonical current policy document.\n\n" +
			"Before any apply, the policy is the empty version-v1 document, which\n" +
			"grants nothing to anyone. The output is valid input to \"acl apply\",\n" +
			"since YAML reads JSON, so the current policy can be captured into the\n" +
			"file a git repository tracks.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			policy, _, err := c.GetPolicy(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to get policy: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(policy)
		},
	}
}
