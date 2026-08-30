// Package rekey provides the CLI endpoint to the "admin rekey" command.
package rekey

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "admin rekey" command used to re-encrypt every secret under a
// new key.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "rekey",
		Short: "Re-encrypt every secret under a new key",
		Long: "Re-encrypt every secret under a new key.\n\n" +
			"The server generates a key, re-seals every secret under it, and starts\n" +
			"using it. This is the way off the key a node was started with, which\n" +
			"matters when that key leaks and as ordinary hygiene.\n\n" +
			"No workload is redeployed. A rekey changes how a value is stored, not\n" +
			"what it is, so no secret's revision moves and nothing reading one is\n" +
			"replaced.\n\n" +
			"Back the keyring up afterwards. The copy taken before this no longer\n" +
			"opens anything the node holds. The key that was replaced is kept, since\n" +
			"it still opens the backups taken before now.",
		Args: cobra.NoArgs,
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
