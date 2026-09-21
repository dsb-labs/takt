// Package apply provides the CLI endpoint to the "service apply" command.
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

// Command returns the "service apply" command used to submit a service manifest to
// the takt server.
func Command() *cobra.Command {
	var ifMatch string

	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a service from a manifest file",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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

			applied, err := c.ApplyService(cmd.Context(), spec, client.WithIfMatch(ifMatch))
			if err != nil {
				return fmt.Errorf("failed to apply service: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(applied)
		},
	}

	cmd.Flags().StringVar(&ifMatch, "if-match", "",
		"apply only if the service's tag still matches this one, as reported by \"takt service get\"")

	return cmd
}
