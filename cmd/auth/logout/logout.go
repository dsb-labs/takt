// Package logout provides the CLI endpoint to the "auth logout" command.
package logout

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/cli"
	"github.com/dsb-labs/takt/pkg/client"
)

// Command returns the "auth logout" command used to revoke the credential
// this client authenticated with.
func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the credential this client authenticated with",
		Long: "Revoke the credential this client authenticated with, whatever its kind,\n" +
			"and remove it from the config file. Self-revocation is always safe, so\n" +
			"this needs no role. The one refusal is the recovery token, whose\n" +
			"revocation path is the reset file in the server's data directory.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.FromContext(cmd.Context())

			if err := c.Logout(cmd.Context()); err != nil {
				return fmt.Errorf("failed to log out: %w", err)
			}

			// The server-side revocation happened either way; a token left in
			// the file would only earn the next command a 401.
			configFlag, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}

			path, err := cli.Path(configFlag)
			if err != nil {
				return err
			}

			settings, err := cli.Load(path)
			if err != nil {
				return err
			}

			if settings.Token == "" {
				return nil
			}

			settings.Token = ""

			return cli.Write(path, settings)
		},
	}
}
