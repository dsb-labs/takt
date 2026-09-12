// Package get provides the CLI endpoint to the "acl get" command.
package get

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "acl get" command used to read the policy document.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Get the policy document",
		Long:  usage,
		Args:  cobra.NoArgs,
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
