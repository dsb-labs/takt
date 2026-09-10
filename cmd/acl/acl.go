// Package acl provides the CLI endpoint to the "acl" command, which groups
// the commands that act on the access-control policy.
package acl

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/acl/apply"
	"github.com/dsb-labs/takt/cmd/acl/get"
	initcmd "github.com/dsb-labs/takt/cmd/acl/init"
)

// Command returns the "acl" command, which does nothing on its own and holds
// the commands that act on the policy.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acl",
		Short: "Initialize and manage the access-control policy",
		Long: "Initialize and manage the access-control policy.\n\n" +
			"The policy is one document binding the three fixed roles — viewer,\n" +
			"operator and admin — to principals and groups. It is applied whole, so a\n" +
			"grant removed from the document is revoked on the next apply, and the\n" +
			"file in a git repository is the complete answer to who holds access.",
	}

	cmd.AddCommand(
		initcmd.Command(),
		get.Command(),
		apply.Command(),
	)

	return cmd
}
