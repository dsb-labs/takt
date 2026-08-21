// Package list provides the CLI endpoint to the "workload list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "workload list" command used to list the workloads known to the orca
// server.
func Command() *cobra.Command {
	var address string
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List workloads",
		Long: "List workloads.\n\n" +
			"Repeat --query to narrow the result; a workload has to match all of them.\n" +
			"A query is a JSON path into the workload's specification and the value it\n" +
			"must hold:\n\n" +
			"  orca workload list --query '$.labels.app=web'\n" +
			"  orca workload list -q '$.labels.app=web' -q '$.labels.env=prod'\n" +
			"  orca workload list -q '$.container.image=nginx:1.27-alpine'\n\n" +
			"Values are compared as text, so a number is matched by its digits\n" +
			"($.ports[0].to=80). A boolean is stored as 1 or 0 and has to be\n" +
			"written that way.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			workloads, err := c.List(cmd.Context(), queries...)
			if err != nil {
				return fmt.Errorf("failed to list workloads: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(workloads)
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.StringArrayVarP(&queries, "query", "q", nil, "filter by a path=value query into the specification, repeatable")

	return cmd
}
