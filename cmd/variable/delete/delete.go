// Package delete provides the CLI endpoint to the "variable delete" command.
package delete

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "variable delete" command used to remove a variable.
func Command() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a variable",
		Long:    usage,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.DeleteVariableOption
			if force {
				options = append(options, client.WithForceDeleteVariable())
			}

			if err := c.DeleteVariable(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to delete variable: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove the variable even though a workload reads it")

	return cmd
}
