// Package health provides the CLI endpoint to the "admin health" command.
package health

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "admin health" command used to check that a server is alive.
func Command() *cobra.Command {
	var wait time.Duration

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check that the server is alive",
		Long: "Check that the server is alive.\n\n" +
			"The command asks the server's health endpoint and exits zero when it\n" +
			"answers. It prints nothing on success: the exit code is the signal,\n" +
			"which is what a script conditions on.\n\n" +
			"With --wait, the endpoint is asked once a second until it answers or\n" +
			"the duration runs out. This is for the moment after starting a server,\n" +
			"when the next step needs it listening.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			if wait <= 0 {
				if err := c.Health(cmd.Context()); err != nil {
					return fmt.Errorf("server is not healthy: %w", err)
				}

				return nil
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), wait)
			defer cancel()

			for {
				// Asked with the bounded context, so a server that accepts the
				// connection and never answers cannot hold the command past its
				// deadline.
				err := c.Health(ctx)
				if err == nil {
					return nil
				}

				select {
				case <-ctx.Done():
					// The interruption is the caller's when the parent context ended
					// it, and the deadline's otherwise. The last refusal says what
					// the server was doing while the command waited.
					if parentErr := cmd.Context().Err(); parentErr != nil {
						return parentErr
					}

					return fmt.Errorf("server was not healthy within %s: %w", wait, err)
				case <-time.After(time.Second):
				}
			}
		},
	}

	cmd.Flags().DurationVar(&wait, "wait", 0, "how long to wait for the server to become healthy, rather than asking once")

	return cmd
}
