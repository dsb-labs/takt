// Package list provides the CLI endpoint to the "service list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "service list" command used to list the services the takt
// server holds.
func Command() *cobra.Command {
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List services",
		Long: "List services.\n\n" +
			"Each service reports its target and the backends it currently selects.\n\n" +
			"Repeat --query to narrow the result. A service has to match all of them.\n" +
			"A query is a JSON path into the service's labels and the value it must\n" +
			"hold:\n\n" +
			"  takt service list --query '$.labels.app=web'\n" +
			"  takt service list -q '$.labels.app=web' -q '$.labels.env=prod'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			services, err := c.ListServices(cmd.Context(), queries...)
			if err != nil {
				return fmt.Errorf("failed to list services: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(services)
		},
	}

	cmd.Flags().StringArrayVarP(&queries, "query", "q", nil, "filter by a path=value query into the labels, repeatable")

	return cmd
}
