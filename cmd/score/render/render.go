// Package render provides the CLI endpoint to the "score render" command.
package render

import (
	_ "embed"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/score"
)

//go:embed usage.txt
var usage string

// Command returns the "score render" command used to render a score's manifests
// without applying them.
func Command() *cobra.Command {
	var (
		values []string
		name   string
	)

	cmd := &cobra.Command{
		Use:   "render <location>",
		Short: "Render a score's manifests and print them",
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

			return write(cmd.OutOrStdout(), rendered)
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&values, "values", "f", nil, "a values file merged over the score's defaults, repeatable, later files winning")
	flags.StringVar(&name, "as", "", "the name to install the score as, which templates read as .Score.Name (default the score's name)")

	return cmd
}

// write prints the rendered documents as one stream, skipping those that
// rendered to nothing.
func write(w io.Writer, rendered score.Rendered) error {
	for _, document := range rendered.Documents {
		if document.Skipped {
			continue
		}

		if _, err := fmt.Fprintf(w, "---\n# Source: %s\n%s", document.Source, document.Text); err != nil {
			return fmt.Errorf("failed to write output: %w", err)
		}
	}

	return nil
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
