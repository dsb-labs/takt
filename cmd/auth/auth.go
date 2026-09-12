// Package auth provides the CLI endpoint to the "auth" command, which groups
// the commands that act on the caller's own authentication.
package auth

import (
	_ "embed"
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/auth/login"
	"github.com/dsb-labs/takt/cmd/auth/logout"
	"github.com/dsb-labs/takt/cmd/auth/whoami"
)

//go:embed usage.txt
var usage string

// Command returns the "auth" command, which does nothing on its own and
// holds the commands that act on the caller's authentication.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Log in, inspect and revoke your own credential",
		Long:  usage,
	}

	cmd.AddCommand(
		login.Command(),
		whoami.Command(),
		logout.Command(),
	)

	return cmd
}
