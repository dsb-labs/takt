// Package apply provides the CLI endpoint to the "acl apply" command.
package apply

import (
	"context"
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

// Command returns the "acl apply" command used to replace the policy with
// the document a file describes.
func Command() *cobra.Command {
	var ifMatch string

	cmd := &cobra.Command{
		Use:   "apply <file>",
		Short: "Replace the policy with a document",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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

			applied, err := apply(cmd.Context(), c, policy, ifMatch)
			if err != nil {
				return fmt.Errorf("failed to apply policy: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(applied)
		},
	}

	cmd.Flags().StringVar(&ifMatch, "if-match", "",
		"apply only if the policy's tag still matches this one, as reported by \"takt acl get\"")

	return cmd
}

// apply replaces the policy against the tag given, or against the tag the
// server reports when none was. Either way the apply is conditional: the
// difference is whether the caller's read or this command's is the one it
// is conditioned on.
func apply(ctx context.Context, c *client.Client, policy manifest.Policy, ifMatch string) (client.Policy, error) {
	if ifMatch == "" {
		return c.ReplacePolicy(ctx, policy)
	}

	return c.ApplyPolicy(ctx, policy, ifMatch)
}
