// Package list provides the CLI endpoint to the "volume list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "volume list" command used to list the volumes the orca server
// holds.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List volumes",
		Long: "List volumes.\n\n" +
			"Each volume reports where its data is and which workloads mount it. A\n" +
			"volume nothing mounts is one that can be deleted without forcing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			volumes, err := c.ListVolumes(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list volumes: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volumes)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
