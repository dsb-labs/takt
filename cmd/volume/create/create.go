// Package create provides the CLI endpoint to the "volume create" command.
package create

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
	"github.com/dsb-labs/orca/pkg/manifest"
)

// Command returns the "volume create" command used to create a volume from a manifest
// file.
func Command() *cobra.Command {
	var address string

	cmd := &cobra.Command{
		Use:   "create <manifest>",
		Short: "Create a volume from a manifest file",
		Long: "Create a volume from a manifest file.\n\n" +
			"A volume has to exist before a workload can mount it, so that a mistyped\n" +
			"name is reported rather than becoming a second empty volume. Creating one\n" +
			"that already exists is refused, because a volume holds data.",
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

			volume, err := c.CreateVolume(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to create volume: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(volume)
		},
	}

	cmd.Flags().StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")

	return cmd
}
