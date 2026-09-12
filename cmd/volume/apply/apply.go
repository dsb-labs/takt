// Package apply provides the CLI endpoint to the "volume apply" command.
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

// Command returns the "volume apply" command used to submit a volume manifest to
// the takt server.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a volume from a manifest file",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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
