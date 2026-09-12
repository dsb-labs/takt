// Package main provides the entrypoint to the takt binary.
//
//go:generate go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
//go:generate go tool mockery
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/cmd/acl"
	"github.com/dsb-labs/takt/cmd/admin"
	"github.com/dsb-labs/takt/cmd/auth"
	"github.com/dsb-labs/takt/cmd/dev"
	"github.com/dsb-labs/takt/cmd/secret"
	"github.com/dsb-labs/takt/cmd/serve"
	"github.com/dsb-labs/takt/cmd/service"
	"github.com/dsb-labs/takt/cmd/token"
	"github.com/dsb-labs/takt/cmd/variable"
	"github.com/dsb-labs/takt/cmd/volume"
	"github.com/dsb-labs/takt/cmd/workload"
	"github.com/dsb-labs/takt/internal/server/driver/exec"
	"github.com/dsb-labs/takt/pkg/cli"
	"github.com/dsb-labs/takt/pkg/client"
)

func main() {
	// Before anything else, because this process may not be takt at all: the exec
	// driver confines a workload by executing this binary, which applies a ruleset to
	// itself and then becomes the workload's command. Nothing set up here would survive
	// that, and the trampoline is not a subcommand, so it is dispatched ahead of cobra
	// rather than added to it.
	exec.Confine()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var address, caCert, configPath string

	cmd := &cobra.Command{
		Use:          "takt",
		Short:        "A single-node workload orchestrator",
		SilenceUsage: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		// The client is built once here and carried in the command context, so
		// no subcommand declares the server flags or builds a client of its
		// own. The serve command gets one it never uses, which costs a URL
		// parse and no connection.
		//
		// The connection resolves from the most specific source that supplies
		// each setting: these flags, then the TAKT_ADDRESS, TAKT_TOKEN and
		// TAKT_CA_CERT environment variables, then the config file `takt auth
		// login` writes.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := cli.Resolve(cli.Sources{ConfigPath: configPath, Address: address, CACert: caCert})
			if err != nil {
				return err
			}

			c, err := client.FromConfig(settings)
			if err != nil {
				return err
			}

			// The settings travel beside the client built from them, for the
			// subcommand that needs the parts a built client no longer shows:
			// login connects without the stored token and saves the address
			// it logged in against.
			cmd.SetContext(client.NewContext(cli.NewContext(cmd.Context(), settings), c))

			return nil
		},
	}

	// The default is resolved at startup so the help output names the real
	// file, TAKT_CONFIG included. A --config value wins over both.
	defaultConfigPath, _ := cli.Path("")

	// The address flag defaults to empty rather than to the default address,
	// because resolution has to tell "not given" from "given the default": a
	// flag pre-filled with an address would shadow the environment and the
	// file on every run. The help text carries the default instead.
	flags := cmd.PersistentFlags()
	flags.StringVarP(&address, "address", "a", "", "URL of the takt server (env TAKT_ADDRESS, default "+cli.DefaultAddress+")")
	flags.StringVar(&caCert, "ca-cert", "", "path to a PEM file holding the certificate authority to check the server against (env TAKT_CA_CERT)")
	flags.StringVar(&configPath, "config", defaultConfigPath, "path of the client config file (env TAKT_CONFIG)")

	if info, ok := debug.ReadBuildInfo(); ok {
		cmd.Version = info.Main.Version
	}

	cmd.AddCommand(
		serve.Command(),
		workload.Command(),
		volume.Command(),
		service.Command(),
		secret.Command(),
		variable.Command(),
		token.Command(),
		acl.Command(),
		auth.Command(),
		admin.Command(),
		dev.Command(),
	)

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
