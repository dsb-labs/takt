// Package logs provides the CLI endpoint to the "logs" command.
package logs

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// Command returns the "logs" command used to read a workload's recent output.
func Command() *cobra.Command {
	var address string
	var tail int

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Read a workload's recent output",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			logs, err := c.Logs(cmd.Context(), args[0], tail)
			if err != nil {
				return fmt.Errorf("failed to read workload logs: %w", err)
			}

			_, err = fmt.Fprint(cmd.OutOrStdout(), logs)

			return err
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.IntVarP(&tail, "tail", "n", 100, "number of lines to read from the end of the logs")

	return cmd
}
