// Package list provides the CLI endpoint to the "secret list" command.
package list

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "secret list" command used to list the secrets the server holds.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the secrets the server holds",
		Long: "List the secrets the server holds, with the workloads reading each one.\n\n" +
			"No value is reported, here or anywhere else. This is how you find out what\n" +
			"exists in order to reference it from a manifest.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			secrets, err := c.ListSecrets(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list secrets: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(secrets)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
