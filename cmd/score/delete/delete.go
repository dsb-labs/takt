// Package delete provides the CLI endpoint to the "score delete" command.
package delete

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/score"
)

//go:embed usage.txt
var usage string

// Command returns the "score delete" command used to remove every resource an
// install of a score applied.
func Command() *cobra.Command {
	var (
		yes     bool
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete every resource an install of a score applied",
		Long:    usage,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			install, err := score.Installed(cmd.Context(), c, args[0])
			if err != nil {
				return fmt.Errorf("failed to find install: %w", err)
			}

			nodes := install.Nodes()
			if len(nodes) == 0 {
				return fmt.Errorf("nothing carries the label of an install named %s", args[0])
			}

			if !yes {
				confirmed, err := confirm(cmd, nodes)
				if err != nil {
					return err
				}

				if !confirmed {
					return errors.New("delete cancelled")
				}
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			deleted, err := score.Destroy(ctx, c, nodes)
			if err != nil {
				printDeleted(cmd.ErrOrStderr(), deleted)
				return fmt.Errorf("failed to delete score: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(deleted)
		},
	}

	flags := cmd.Flags()
	flags.BoolVarP(&yes, "yes", "y", false, "delete without asking first")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for the whole delete, workload teardown included")

	return cmd
}

// confirm prints what will be deleted and asks whether to go on, refusing to
// guess when there is no terminal to ask.
func confirm(cmd *cobra.Command, nodes []score.Node) (bool, error) {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, errors.New("no terminal to confirm on: pass --yes to delete without asking")
	}

	fmt.Fprintln(cmd.ErrOrStderr(), "This will delete:")
	for _, node := range nodes {
		fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", node)
	}

	fmt.Fprint(cmd.ErrOrStderr(), "Volumes are deleted with the data they hold. Continue? [y/N] ")

	answer, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("failed to read the answer: %w", err)
	}

	return strings.EqualFold(strings.TrimSpace(answer), "y"), nil
}

// printDeleted says what went before a failed delete, so an operator knows
// what state the server was left in.
func printDeleted(w io.Writer, deleted []score.Node) {
	if len(deleted) == 0 {
		return
	}

	fmt.Fprintln(w, "deleted before the failure:")
	for _, node := range deleted {
		fmt.Fprintf(w, "  %s\n", node)
	}
}
