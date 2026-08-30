// Package get provides the CLI endpoint to the "volume get" command.
package get

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "volume get" command used to show a single volume.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a single volume",
		Long: "Show a single volume.\n\n" +
			"Reports where the volume's data is on the host running the server, and the\n" +
			"workloads currently mounting it.",
		Args: cobra.ExactArgs(1),
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
