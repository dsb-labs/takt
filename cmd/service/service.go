// Package service provides the CLI endpoint to the "service" command, which groups
// the commands that act on services.
package service

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/service/apply"
	delcmd "github.com/dsb-labs/takt/cmd/service/delete"
	"github.com/dsb-labs/takt/cmd/service/get"
	"github.com/dsb-labs/takt/cmd/service/list"
)

//go:embed usage.txt
var usage string

// Command returns the "service" command, which does nothing on its own and holds
// the commands that act on a service.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Create, inspect and delete services",
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
