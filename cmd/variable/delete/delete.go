// Package delete provides the CLI endpoint to the "variable delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "variable delete" command used to remove a variable.
func Command() *cobra.Command {
	var address string
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a variable",
		Long: "Delete a variable.\n\n" +
			"A variable a workload reads is refused, and the workloads reading it are\n" +
			"named; pass --force to remove it anyway. Those workloads keep running until\n" +
			"something replaces them, and then cannot start until the variable exists\n" +
			"again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			var options []client.DeleteVariableOption
			if force {
				options = append(options, client.WithForceDeleteVariable())
			}

			if err = c.DeleteVariable(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to delete variable: %w", err)
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.BoolVarP(&force, "force", "f", false, "remove the variable even though a workload reads it")

	return cmd
}
