// Package whoami provides the CLI endpoint to the "auth whoami" command.
package whoami

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "auth whoami" command used to report the caller's own
// identity.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Report who the server thinks you are",
		Long:  usage,
		Args:  cobra.NoArgs,
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
