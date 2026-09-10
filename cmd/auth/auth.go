// Package auth provides the CLI endpoint to the "auth" command, which groups
// the commands that act on the caller's own authentication.
package auth

import (
	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/auth/login"
	"github.com/dsb-labs/takt/cmd/auth/logout"
	"github.com/dsb-labs/takt/cmd/auth/whoami"
)

// Command returns the "auth" command, which does nothing on its own and
// holds the commands that act on the caller's authentication.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Log in, inspect and revoke your own credential",
		Long: "Log in, inspect and revoke your own credential.\n\n" +
			"Login exchanges an OIDC identity for a short-lived token and writes it\n" +
			"to the config file, so the commands that follow present it without\n" +
			"ceremony. Whoami reports who the server thinks you are. Logout revokes\n" +
			"whatever credential authenticated the call.",
	}

	cmd.AddCommand(
		login.Command(),
		whoami.Command(),
		logout.Command(),
	)

	return cmd
}
