// Package apply provides the CLI endpoint to the "acl apply" command.
package apply

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

//go:embed usage.txt
var usage string

// Command returns the "acl apply" command used to replace the policy with
// the document a file describes.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <file>",
		Short: "Replace the policy with a document",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("failed to open manifest: %w", err)
			}
			defer f.Close()

			policy, err := manifest.ParsePolicy(f)
			if err != nil {
				return err
			}

			c := client.FromContext(cmd.Context())

			applied, _, err := c.ReplacePolicy(cmd.Context(), policy)
			if err != nil {
				return fmt.Errorf("failed to apply policy: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(applied)
		},
	}
}
