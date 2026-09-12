// Package list provides the CLI endpoint to the "variable list" command.
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

// Command returns the "variable list" command used to list the variables the server
// holds.
func Command() *cobra.Command {
	var queries []string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the variables the server holds",
		Long:    usage,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			variables, err := c.ListVariables(cmd.Context(), queries...)
			if err != nil {
				return fmt.Errorf("failed to list variables: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(variables)
		},
	}

	cmd.Flags().StringArrayVarP(&queries, "query", "q", nil, "filter by a path=value query into the labels, repeatable")

	return cmd
}
