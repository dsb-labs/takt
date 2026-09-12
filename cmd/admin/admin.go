// Package admin provides the CLI endpoint to the "admin" command, which groups the
// commands that act on the node itself.
package admin

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/admin/backup"
	"github.com/dsb-labs/takt/cmd/admin/health"
	"github.com/dsb-labs/takt/cmd/admin/rekey"
	"github.com/dsb-labs/takt/cmd/admin/restore"
)

//go:embed usage.txt
var usage string

// Command returns the "admin" command, which does nothing on its own and holds the
// commands that act on the node rather than on the workloads running on it.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operate on the node itself",
		Long:  usage,
	}

	cmd.AddCommand(
		backup.Command(),
		health.Command(),
		rekey.Command(),
		restore.Command(),
	)

	return cmd
}
