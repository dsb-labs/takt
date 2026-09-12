// Package volume provides the CLI endpoint to the "volume" command, which groups the
// commands that act on volumes.
package volume

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/volume/apply"
	delcmd "github.com/dsb-labs/takt/cmd/volume/delete"
	"github.com/dsb-labs/takt/cmd/volume/get"
	"github.com/dsb-labs/takt/cmd/volume/list"
)

//go:embed usage.txt
var usage string

// Command returns the "volume" command, which does nothing on its own and holds the
// commands that act on a volume.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Create, inspect and delete volumes",
		Long:  usage,
	}

	cmd.AddCommand(
		apply.Command(),
		list.Command(),
		get.Command(),
		delcmd.Command(),
	)

	return cmd
}
