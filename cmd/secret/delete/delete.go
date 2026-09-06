// Package delete provides the CLI endpoint to the "secret delete" command.
package delete

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "secret delete" command used to remove a secret.
func Command() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a secret",
		Long: "Delete a secret.\n\n" +
			"A secret a workload reads is refused, and the workloads reading it are\n" +
			"named; pass --force to remove it anyway. Those workloads keep running until\n" +
			"something replaces them, and then cannot start until the secret exists\n" +
			"again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			var options []client.DeleteSecretOption
			if force {
				options = append(options, client.WithForceDelete())
			}

			if err := c.DeleteSecret(cmd.Context(), args[0], options...); err != nil {
				return fmt.Errorf("failed to delete secret: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove the secret even though a workload reads it")

	return cmd
}
