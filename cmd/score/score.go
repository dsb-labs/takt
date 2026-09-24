// Package score provides the CLI endpoint to the "score" command, which groups
// the commands that act on scores.
package score

import (
	_ "embed"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/score/apply"
	delcmd "github.com/dsb-labs/takt/cmd/score/delete"
	"github.com/dsb-labs/takt/cmd/score/list"
	"github.com/dsb-labs/takt/cmd/score/render"
	"github.com/dsb-labs/takt/cmd/score/show"
)

//go:embed usage.txt
var usage string

// Command returns the "score" command, which does nothing on its own and holds
// the commands that act on a score.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "score",
		Short: "Render, inspect, apply, list and delete scores",
		Long:  usage,
	}

	cmd.AddCommand(
		render.Command(),
		show.Command(),
		apply.Command(),
		delcmd.Command(),
		list.Command(),
	)

	return cmd
}
