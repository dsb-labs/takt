// Package apply provides the CLI endpoint to the "service apply" command.
package apply

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// Command returns the "service apply" command used to submit a service manifest to
// the takt server.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a service from a manifest file",
		Long: "Create or update a service from a manifest file.\n\n" +
			"The stored selection becomes what the manifest says, however many times\n" +
			"it is applied. The workloads the target selects do not have to exist: a\n" +
			"service applied ahead of its workloads reports no backends until they\n" +
			"arrive.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("failed to open manifest: %w", err)
			}
			defer f.Close()

			spec, err := manifest.ParseService(f)
			if err != nil {
				return err
			}

			c := client.FromContext(cmd.Context())

			applied, err := c.ApplyService(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to apply service: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(applied)
		},
	}

	return cmd
}
