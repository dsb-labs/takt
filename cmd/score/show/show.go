// Package show provides the CLI endpoint to the "score show" command.
package show

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/score"
)

//go:embed usage.txt
var usage string

// Command returns the "score show" command used to report what a score contains
// once rendered.
func Command() *cobra.Command {
	var (
		values []string
		name   string
	)

	cmd := &cobra.Command{
		Use:   "show <location>",
		Short: "Show what a score contains once rendered",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var options []score.Option
			if name != "" {
				options = append(options, score.WithName(name))
			}

			rendered, err := score.Build(args[0], values, options...)
			if err != nil {
				return fmt.Errorf("failed to render score: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(score.Summarise(rendered))
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&values, "values", "f", nil, "a values file merged over the score's defaults, repeatable, later files winning")
	flags.StringVar(&name, "as", "", "the name to install the score as, which templates read as .Score.Name (default the score's name)")

	return cmd
}
