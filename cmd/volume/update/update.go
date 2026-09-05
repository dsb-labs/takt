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

// Command returns the "volume update" command used to change a volume's mutable
// fields from a manifest file.
func Command() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "update <manifest>",
		Short: "Update a volume from a manifest file",
		Long: "Update a volume from a manifest file.\n\n" +
			"The labels, the owner and the mode are the whole of what this changes. A\n" +
			"volume's name identifies it, the directory holding its data is named for\n" +
			"the identifier it was assigned, and its contents are the workloads' to\n" +
			"write.\n\n" +
			"The fields in the manifest replace the ones stored, the way applying a\n" +
			"workload manifest replaces a workload's. A manifest carrying no labels\n" +
			"removes them all. The owner and mode are applied to the directory again,\n" +
			"which is how a live volume is handed to another user. A manifest clearing\n" +
			"either leaves the directory as it stands.\n\n" +
			"Nothing mounting the volume is redeployed, because none of these fields\n" +
			"reach a workload's specification hash.",
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

			c := client.FromContext(cmd.Context())

			volume, err := c.UpdateVolume(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to update volume: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volume)
		},
	}

	return cmd
}
