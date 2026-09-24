// Package list provides the CLI endpoint to the "score list" command.
package list

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/score"
)

//go:embed usage.txt
var usage string

// Command returns the "score list" command used to list the installs of scores
// the server holds.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the installs of scores the server holds",
		Long:    usage,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			installs, err := score.List(cmd.Context(), c)
			if err != nil {
				return fmt.Errorf("failed to list scores: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(installs)
		},
	}
}
