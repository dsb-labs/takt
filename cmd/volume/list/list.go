// Package list provides the CLI endpoint to the "volume list" command.
package list

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "volume list" command used to list the volumes the takt server
// holds.
func Command() *cobra.Command {
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List volumes",
		Long:    usage,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			volumes, err := c.ListVolumes(cmd.Context(), queries...)
			if err != nil {
				return fmt.Errorf("failed to list volumes: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volumes)
		},
	}

	cmd.Flags().StringArrayVarP(&queries, "query", "q", nil, "filter by a path=value query into the labels, repeatable")

	return cmd
}
