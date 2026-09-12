// Package get provides the CLI endpoint to the "volume get" command.
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

// Command returns the "volume get" command used to show a single volume.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a single volume",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			volume, err := c.GetVolume(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get volume: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volume)
		},
	}

	return cmd
}
