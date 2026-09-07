// Package apply provides the CLI endpoint to the "volume apply" command.
package apply

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// Command returns the "volume apply" command used to submit a volume manifest to
// the takt server.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a volume from a manifest file",
		Long: "Create or update a volume from a manifest file.\n\n" +
			"The stored volume becomes what the manifest says, however many times it\n" +
			"is applied. A volume has to exist before a workload can mount it, so that\n" +
			"a mistyped name is reported rather than becoming a second empty volume.\n\n" +
			"The directory keeps its path across an apply, so nothing mounting the\n" +
			"volume is redeployed, and the owner and mode are reapplied to it. An\n" +
			"owner or a mode the manifest leaves empty stops being enforced rather\n" +
			"than being reverted.",
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

			applied, err := c.ApplyVolume(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("failed to apply volume: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(applied)
		},
	}

	return cmd
}
