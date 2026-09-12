// Package list provides the CLI endpoint to the "workload list" command.
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

// Command returns the "workload list" command used to list the workloads known to the takt
// server.
func Command() *cobra.Command {
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List workloads",
		Long:    usage,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			workloads, err := c.List(cmd.Context(), queries...)
			if err != nil {
				return fmt.Errorf("failed to list workloads: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(workloads)
		},
	}

	cmd.Flags().StringArrayVarP(&queries, "query", "q", nil, "filter by a path=value query into the specification, repeatable")

	return cmd
}
