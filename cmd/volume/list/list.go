// Package list provides the CLI endpoint to the "volume list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "volume list" command used to list the volumes the takt server
// holds.
func Command() *cobra.Command {
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List volumes",
		Long: "List volumes.\n\n" +
			"Each volume reports where its data is and which workloads mount it. A\n" +
			"volume nothing mounts is one that can be deleted without forcing.\n\n" +
			"Repeat --query to narrow the result. A volume has to match all of them.\n" +
			"A query is a JSON path into the volume's labels and the value it must\n" +
			"hold:\n\n" +
			"  takt volume list --query '$.labels.app=web'\n" +
			"  takt volume list -q '$.labels.app=web' -q '$.labels.env=prod'",
		Args: cobra.NoArgs,
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
