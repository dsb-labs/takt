// Package apply provides the CLI endpoint to the "workload apply" command.
package apply

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

//go:embed usage.txt
var usage string

// Command returns the "workload apply" command used to submit a workload manifest to the
// takt server.
func Command() *cobra.Command {
	var (
		dryRun  bool
		ifMatch string
		wait    bool
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Create or update a workload from a manifest file",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
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

			ctx := cmd.Context()
			options := []client.ApplyOption{client.WithIfMatch(ifMatch)}

			if wait {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()

				options = append(options, client.WithWaitForRollout())
			}

			workload, _, err := c.Apply(ctx, spec, options...)
			if err != nil {
				return fmt.Errorf("failed to apply workload: %w", err)
			}

			return enc.Encode(workload)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&ifMatch, "if-match", "",
		"apply only if the workload's tag still matches this one, as reported by \"takt workload get\"")
	flags.BoolVar(&dryRun, "dry-run", false, "report what applying the manifest would do, and apply nothing")
	flags.BoolVarP(&wait, "wait", "w", false, "block until the workload has settled on the manifest")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for the workload to settle, with --wait")

	return cmd
}
