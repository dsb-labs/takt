// Package node provides the CLI endpoint to the "node" command, which groups the
// commands that act on the machine the server runs on.
package node

import (
	_ "embed"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/node/get"
)

//go:embed usage.txt
var usage string

// Command returns the "node" command, which does nothing on its own and holds
// the commands that act on the node.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Inspect the node the server runs on",
		Long:  usage,
	}

	cmd.AddCommand(
		get.Command(),
	)

	return cmd
}
