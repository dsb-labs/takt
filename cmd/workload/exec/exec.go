// Package exec provides the CLI endpoint to the "workload exec" command.
package exec

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/cli"
	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "workload exec" command used to run a command inside a
// running instance of a workload.
func Command() *cobra.Command {
	var instance int
	var stdin bool

	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command inside a running instance of a workload",
		Long:  usage,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			command := client.Command{
				Command: args[1:],
				Stdout:  cmd.OutOrStdout(),
				Stderr:  cmd.ErrOrStderr(),
			}

			if cmd.Flags().Changed("instance") {
				command.Instance = &instance
			}

			if stdin {
				command.Stdin = cmd.InOrStdin()
			}

			status, err := c.Exec(cmd.Context(), args[0], command)
			switch {
			case errors.Is(err, context.Canceled):
				// Interrupting the command is how an operator ends one that was
				// not going to end on its own, and they are the one who stopped it.
				return nil
			case err != nil:
				return fmt.Errorf("failed to run command: %w", err)
			case status != 0:
				return cli.ExitError{Code: status}
			default:
				return nil
			}
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&instance, "instance", "i", 0, "index of the instance to run the command in, for a workload running more than one")
	flags.BoolVarP(&stdin, "stdin", "s", false, "attach this process's standard input to the command")

	// Flags after the double dash belong to the command being run, so cobra has
	// to stop reading them there rather than refusing ones it does not know.
	cmd.Flags().SetInterspersed(false)

	return cmd
}
