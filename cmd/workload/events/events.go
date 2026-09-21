// Package events provides the CLI endpoint to the "workload events" command.
package events

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload events" command used to read what the server
// recorded about a workload.
func Command() *cobra.Command {
	var limit int
	var since string

	cmd := &cobra.Command{
		Use:   "events <name>",
		Short: "Read what the server recorded about a workload",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			options := []client.EventOption{client.WithEventsLimit(limit)}
			if since != "" {
				instant, err := instant(since, time.Now())
				if err != nil {
					return err
				}

				options = append(options, client.WithEventsSince(instant))
			}

			events, err := c.Events(cmd.Context(), args[0], options...)
			if err != nil {
				return fmt.Errorf("failed to get workload events: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(events)
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&limit, "limit", "n", 100, "number of events to read, most recently seen first")
	flags.StringVar(&since, "since", "", "read only the events last seen since a duration ago or an RFC 3339 time")

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
