// Package events provides the CLI endpoint to the "workload events" command.
package events

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload events" command used to read what the server
// recorded about a workload.
func Command() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "events <name>",
		Short: "Read what the server recorded about a workload",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			events, err := c.Events(cmd.Context(), args[0], limit)
			if err != nil {
				return fmt.Errorf("failed to get workload events: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(events)
		},
	}

	cmd.Flags().IntVarP(&limit, "limit", "n", 100, "number of events to read, most recently seen first")

	return cmd
}
