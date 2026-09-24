package apply

import (
	"bufio"
	"context"
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

// pruneInstall deletes what carries the install's label that the rendered
// score no longer names, asking first unless told not to.
func pruneInstall(cmd *cobra.Command, c *client.Client, rendered score.Rendered, yes bool, timeout time.Duration) ([]score.Node, error) {
	install, err := score.Installed(cmd.Context(), c, rendered.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to find install: %w", err)
	}

	nodes := score.Prunable(rendered, install)
	if len(nodes) == 0 {
		return []score.Node{}, nil
	}

	if !yes {
		confirmed, err := confirm(cmd, nodes)
		if err != nil {
			return nil, err
		}

		if !confirmed {
			return nil, errors.New("prune cancelled")
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	pruned, err := score.Destroy(ctx, c, nodes)
	if err != nil {
		printNodes(cmd.ErrOrStderr(), "pruned before the failure:", pruned)
		return nil, fmt.Errorf("failed to prune score: %w", err)
	}

	return pruned, nil
}

// confirm prints what will be pruned and asks whether to go on, refusing to
// guess when there is no terminal to ask.
func confirm(cmd *cobra.Command, nodes []score.Node) (bool, error) {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, errors.New("no terminal to confirm on: pass --yes to prune without asking")
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
