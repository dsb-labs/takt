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
	var address string

	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a workload from a manifest file",
		Long: "Create or update a workload from a manifest file.\n\n" +
			"Applying the same manifest twice is a no-op: the workload's version only\n" +
			"changes when its specification does.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("failed to open manifest: %w", err)
			}
			defer f.Close()

			spec, err := manifest.Parse(f)
			if err != nil {
				return err
			}

			c, err := client.New(address)
			if err != nil {
				return err
			}

			workload, _, err := c.Apply(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to apply workload: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(workload)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
