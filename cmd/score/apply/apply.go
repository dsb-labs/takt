// Package apply provides the CLI endpoint to the "score apply" command.
package apply

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/score"
)

//go:embed usage.txt
var usage string

// The result type is what a successful apply prints: what was applied, and
// what pruning removed.
type result struct {
	// The resources applied, in order.
	Applied []score.Node
	// The resources pruned, in order. Empty without --prune.
	Pruned []score.Node
}

// Command returns the "score apply" command used to render a score and apply
// every resource in it.
func Command() *cobra.Command {
	var (
		values  []string
		secrets []string
		name    string
		adopt   bool
		noInput bool
		prune   bool
		yes     bool
		timeout time.Duration
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

			preconditions, err := score.Check(cmd.Context(), c, rendered)
			if err != nil {
				return fmt.Errorf("failed to check score: %w", err)
			}

			if err = setSecrets(cmd, c, rendered, preconditions.Missing, secrets, noInput); err != nil {
				return err
			}

			report, err := score.Apply(cmd.Context(), c, rendered, applyOptions...)
			if err != nil {
				printNodes(cmd.ErrOrStderr(), "applied before the failure:", report.Applied)
				return fmt.Errorf("failed to apply score: %w", err)
			}

			out := result{Applied: report.Applied, Pruned: []score.Node{}}
			if prune {
				out.Pruned, err = pruneInstall(cmd, c, rendered, yes, timeout)
				if err != nil {
					return err
				}
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(out)
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&values, "values", "f", nil, "a values file merged over the score's defaults, repeatable, later files winning")
	flags.StringVar(&name, "as", "", "the name to install the score as, which templates read as .Score.Name (default the score's name)")
	flags.BoolVar(&adopt, "adopt", false, "take over resources that exist without this score's label rather than refusing them")
	flags.StringArrayVar(&secrets, "secret", nil, "a declared secret to set from a file if it is missing, as name=@path, repeatable")
	flags.BoolVar(&noInput, "no-input", false, "never prompt for a missing secret, and fail naming every one instead")
	flags.BoolVar(&prune, "prune", false, "after applying, delete what carries this install's label that the score no longer names")
	flags.BoolVarP(&yes, "yes", "y", false, "prune without asking first")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for a prune, workload teardown included")

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

// printNodes lists nodes under a heading, for saying what landed or what went
// before a failure so an operator knows what state the server was left in.
func printNodes(w io.Writer, heading string, nodes []score.Node) {
	if len(nodes) == 0 {
		return
	}

	fmt.Fprintln(w, heading)
	for _, node := range nodes {
		fmt.Fprintf(w, "  %s\n", node)
	}
}
