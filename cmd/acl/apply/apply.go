// Package apply provides the CLI endpoint to the "acl apply" command.
package apply

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// Command returns the "acl apply" command used to replace the policy with
// the document a file describes.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <file>",
		Short: "Replace the policy with a document",
		Long: "Replace the whole policy with the document in the given file.\n\n" +
			"The apply is atomic and complete: a grant absent from the file is\n" +
			"revoked, with no prune step, and the change applies to the very next\n" +
			"request. The document is read, then applied on the condition that the\n" +
			"policy did not change in between, so two concurrent applies produce one\n" +
			"winner and one error to re-run rather than a silent overwrite.\n\n" +
			"Requires the admin role or the recovery token, which is what makes a\n" +
			"bad apply recoverable: the recovery token sits above the policy it\n" +
			"fixes.",
		Args: cobra.ExactArgs(1),
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
