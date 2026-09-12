// Package acl provides the CLI endpoint to the "acl" command, which groups
// the commands that act on the access-control policy.
package acl

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/acl/apply"
	"github.com/dsb-labs/takt/cmd/acl/get"
	initcmd "github.com/dsb-labs/takt/cmd/acl/init"
)

//go:embed usage.txt
var usage string

// Command returns the "acl" command, which does nothing on its own and holds
// the commands that act on the policy.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acl",
		Short: "Initialize and manage the access-control policy",
		Long:  usage,
	}

	cmd.AddCommand(
		initcmd.Command(),
		get.Command(),
		apply.Command(),
	)

	return cmd
}
