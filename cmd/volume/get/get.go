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
	var address string

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a single volume",
		Long: "Show a single volume.\n\n" +
			"Reports where the volume's data is on the host running the server, and the\n" +
			"workloads currently mounting it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			volume, err := c.GetVolume(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get volume: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volume)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
