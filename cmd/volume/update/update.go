// Package update provides the CLI endpoint to the "volume update" command.
package update

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// Command returns the "volume update" command used to change a volume's labels from a
// manifest file.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:   "update <manifest>",
		Short: "Update a volume's labels from a manifest file",
		Long: "Update a volume's labels from a manifest file.\n\n" +
			"Labels are the whole of what this changes. A volume's name identifies it,\n" +
			"the directory holding its data is named for the identifier it was assigned,\n" +
			"and its contents are the workloads' to write.\n\n" +
			"The labels in the manifest replace the ones stored, the way applying a\n" +
			"workload manifest replaces a workload's. A manifest carrying none removes\n" +
			"them all.\n\n" +
			"Nothing mounting the volume is redeployed. A label says nothing about the\n" +
			"storage, so no specification hash moves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("failed to open manifest: %w", err)
			}
			defer f.Close()

			spec, err := manifest.ParseVolume(f)
			if err != nil {
				return err
			}

			c, err := client.New(address)
			if err != nil {
				return err
			}

			volume, err := c.UpdateVolume(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to update volume: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volume)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
