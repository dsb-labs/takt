// Package apply provides the CLI endpoint to the "score apply" command.
package apply

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/score"
)

//go:embed usage.txt
var usage string

// Command returns the "score apply" command used to render a score and apply
// every resource in it.
func Command() *cobra.Command {
	var (
		values []string
		name   string
		adopt  bool
	)

	cmd := &cobra.Command{
		Use:   "apply <location>",
		Short: "Render a score and apply every resource in it",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var options []score.Option
			if name != "" {
				options = append(options, score.WithName(name))
			}

			rendered, err := score.Build(args[0], values, options...)
			if err != nil {
				printFailure(cmd.ErrOrStderr(), rendered, err)
				return fmt.Errorf("failed to render score: %w", err)
			}

			var applyOptions []score.ApplyOption
			if adopt {
				applyOptions = append(applyOptions, score.WithAdopt())
			}

			c := client.FromContext(cmd.Context())

			report, err := score.Apply(cmd.Context(), c, rendered, applyOptions...)
			if err != nil {
				printReport(cmd.ErrOrStderr(), report)
				return fmt.Errorf("failed to apply score: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(report)
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&values, "values", "f", nil, "a values file merged over the score's defaults, repeatable, later files winning")
	flags.StringVar(&name, "as", "", "the name to install the score as, which templates read as .Score.Name (default the score's name)")
	flags.BoolVar(&adopt, "adopt", false, "take over resources that exist without this score's label rather than refusing them")

	return cmd
}

// printFailure prints the document a render failed in with line numbers, when
// the failure was in a document rather than in reaching one.
func printFailure(w io.Writer, rendered score.Rendered, err error) {
	if !errors.Is(err, score.ErrInvalidDocument) || len(rendered.Documents) == 0 {
		return
	}

	document := rendered.Documents[len(rendered.Documents)-1]
	fmt.Fprintf(w, "# %s\n%s\n", document.Source, document.Numbered())
}

// printReport says what landed before a failed apply, so an operator knows
// what state the server was left in.
func printReport(w io.Writer, report score.Report) {
	if len(report.Applied) == 0 {
		return
	}

	fmt.Fprintln(w, "applied before the failure:")
	for _, node := range report.Applied {
		fmt.Fprintf(w, "  %s\n", node)
	}
}
