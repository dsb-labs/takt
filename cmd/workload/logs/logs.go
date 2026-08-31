// Package logs provides the CLI endpoint to the "workload logs" command.
package logs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "workload logs" command used to read a workload's recent output.
func Command() *cobra.Command {
	var tail int
	var previous bool
	var follow bool
	var since string
	var instance int

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Read a workload's recent output",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			options := []client.LogOption{client.WithTail(tail)}
			if previous {
				options = append(options, client.WithPrevious())
			}

			if cmd.Flags().Changed("instance") {
				options = append(options, client.WithInstance(instance))
			}

			if follow {
				options = append(options, client.WithFollow())
			}

			if since != "" {
				instant, err := instant(since, time.Now())
				if err != nil {
					return err
				}

				options = append(options, client.WithSince(instant))
			}

			if err := c.Logs(cmd.Context(), cmd.OutOrStdout(), args[0], options...); err != nil {
				// Interrupting a follow is how most of them end, so it leaves the
				// command successful. The output already written is what the caller
				// asked for, and they are the one who stopped it.
				if errors.Is(err, context.Canceled) {
					return nil
				}

				return fmt.Errorf("failed to read workload logs: %w", err)
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&tail, "tail", "n", 100, "number of lines to read from the end of the logs")
	flags.BoolVarP(&previous, "previous", "p", false, "read the instance that was replaced rather than the one running now")
	flags.BoolVarP(&follow, "follow", "f", false, "keep reading output until the instance ends")
	flags.StringVar(&since, "since", "", "read only the output written since a duration ago or an RFC 3339 time, for container workloads")
	flags.IntVarP(&instance, "instance", "i", 0, "index of the instance to read, for a workload running more than one")

	// The instance a replacement kept has already ended, so there is nothing for a
	// follow of it to wait on.
	cmd.MarkFlagsMutuallyExclusive("follow", "previous")

	return cmd
}

// instant turns what the operator typed into the moment they meant.
//
// Both forms are accepted because they answer different questions. A duration is what
// somebody looking at a workload right now types, and an absolute time is what somebody
// correlating with another record has. The API carries only the absolute one, since a
// duration means nothing once the request has been sent.
func instant(value string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(value); err == nil {
		// A duration says how long ago. A negative one would name a moment in the
		// future, and a read that returns nothing is a worse answer than being told
		// what was wrong with the request.
		if d < 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: a duration says how long ago, so it cannot be negative", value)
		}

		return now.Add(-d), nil
	}

	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q: use a duration such as 10m or an RFC 3339 time", value)
	}

	return t, nil
}
