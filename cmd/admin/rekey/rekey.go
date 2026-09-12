// Package rekey provides the CLI endpoint to the "admin rekey" command.
package rekey

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "admin rekey" command used to re-encrypt every secret under a
// new key.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "rekey",
		Short: "Re-encrypt every secret under a new key",
		Long:  usage,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			rekey, err := c.Rekey(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to rekey: %w", err)
			}

			// On stderr so that stdout stays a document something else can read. An
			// operator who does not take a fresh backup now has one that opens
			// nothing.
			fmt.Fprintln(cmd.ErrOrStderr(),
				"The keyring has changed. Back it up now, because the copy you had opens nothing.")

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(rekey)
		},
	}

	return cmd
}
