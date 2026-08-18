// Package delete provides the CLI endpoint to the "delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "delete" command used to remove a workload and stop
// everything running for it.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a workload and stop its work",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			if err = c.Delete(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("failed to delete workload: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
