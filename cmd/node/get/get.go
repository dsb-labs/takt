// Package get provides the CLI endpoint to the "node get" command.
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

// Command returns the "node get" command used to show the node.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Show the node",
		Long:  usage,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			node, err := c.GetNode(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to get node: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(node)
		},
	}

	return cmd
}
