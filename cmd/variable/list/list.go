// Package list provides the CLI endpoint to the "variable list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "variable list" command used to list the variables the server
// holds.
func Command() *cobra.Command {
	var address string
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the variables the server holds",
		Long: "List the variables the server holds, with the value of each one and the\n" +
			"workloads reading it.\n\n" +
			"The values are reported, unlike a secret's. Reviewing what a fleet is\n" +
			"configured with is the reason to choose a variable, so a listing that\n" +
			"withheld them would defeat the point.\n\n" +
			"Repeat --query to narrow the result. A variable has to match all of them.\n" +
			"A query is a JSON path into the variable's labels and the value it must\n" +
			"hold:\n\n" +
			"  orca variable list --query '$.labels.app=web'\n" +
			"  orca variable list -q '$.labels.app=web' -q '$.labels.env=prod'",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			variables, err := c.ListVariables(cmd.Context(), queries...)
			if err != nil {
				return fmt.Errorf("failed to list variables: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(variables)
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.StringArrayVarP(&queries, "query", "q", nil, "filter by a path=value query into the labels, repeatable")

	return cmd
}
