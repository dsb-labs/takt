// Package logout provides the CLI endpoint to the "auth logout" command.
package logout

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/cli"
	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "auth logout" command used to revoke the credential
// this client authenticated with.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the credential this client authenticated with",
		Long:  usage,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			if err := c.Logout(cmd.Context()); err != nil {
				return fmt.Errorf("failed to log out: %w", err)
			}

			// The server-side revocation happened either way; a token left in
			// the file would only earn the next command a 401. The file is
			// read afresh rather than taken from the resolved settings, so a
			// token that came from the environment does not have the rest of
			// the resolution written into the file on its way out.
			stored, err := cli.Load(cli.FromContext(cmd.Context()).File)
			if err != nil {
				return err
			}

			if stored.Token == "" {
				return nil
			}

			stored.Token = ""

			return stored.Save()
		},
	}
}
