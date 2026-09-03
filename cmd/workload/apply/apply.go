// Package apply provides the CLI endpoint to the "workload apply" command.
package apply

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// Command returns the "workload apply" command used to submit a workload manifest to the
// orca server.
func Command() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a workload from a manifest file",
		Long: "Create or update a workload from a manifest file.\n\n" +
			"Applying the same manifest twice is a no-op: the workload's version only\n" +
			"changes when its specification does.\n\n" +
			"Use --dry-run to report what applying the manifest would do without doing\n" +
			"any of it. The report says whether the workload would be created, whether\n" +
			"its running instances would be replaced, and which fields would change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("failed to open manifest: %w", err)
			}
			defer f.Close()

			spec, err := manifest.ParseWorkload(f)
			if err != nil {
				return err
			}

			c := client.FromContext(cmd.Context())

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			if dryRun {
				run, err := c.DryRun(cmd.Context(), spec)
				if err != nil {
					return fmt.Errorf("failed to dry run workload: %w", err)
				}

				return enc.Encode(run)
			}

			workload, _, err := c.Apply(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to apply workload: %w", err)
			}

			return enc.Encode(workload)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what applying the manifest would do, and apply nothing")

	return cmd
}
