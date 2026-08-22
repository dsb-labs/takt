// Package get provides the CLI endpoint to the "secret get" command.
package get

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "secret get" command used to read a single secret.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a single secret",
		Long: "Get a single secret, with the workloads reading it.\n\n" +
			"The value is not part of the output. Nothing reads a secret back out of\n" +
			"orca: once set, the only thing that sees the value is a workload being\n" +
			"started. What is reported is the revision, which changes whenever the value\n" +
			"changes, so a rotation can be confirmed without the value being shown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			secret, err := c.GetSecret(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to get secret: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(secret)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
