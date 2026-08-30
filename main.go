// Package main provides the entrypoint to the orca binary.
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

	"github.com/dsb-labs/orca/cmd/admin"
	"github.com/dsb-labs/orca/cmd/dev"
	"github.com/dsb-labs/orca/cmd/secret"
	"github.com/dsb-labs/orca/cmd/serve"
	"github.com/dsb-labs/orca/cmd/variable"
	"github.com/dsb-labs/orca/cmd/volume"
	"github.com/dsb-labs/orca/cmd/workload"
	"github.com/dsb-labs/orca/internal/server/driver/exec"
	"github.com/dsb-labs/orca/pkg/client"
)

func main() {
	// Before anything else, because this process may not be orca at all: the exec
	// driver confines a workload by executing this binary, which applies a ruleset to
	// itself and then becomes the workload's command. Nothing set up here would survive
	// that, and the trampoline is not a subcommand, so it is dispatched ahead of cobra
	// rather than added to it.
	exec.Confine()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var address, caCert string

	cmd := &cobra.Command{
		Use:          "orca",
		Short:        "A single-node workload orchestrator",
		SilenceUsage: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		// The client is built once here and carried in the command context, so
		// no subcommand declares the server flags or builds a client of its
		// own. The serve command gets one it never uses, which costs a URL
		// parse and no connection.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			var options []client.Option
			if caCert != "" {
				options = append(options, client.WithCACertificate(caCert))
			}

			c, err := client.New(address, options...)
			if err != nil {
				return err
			}

			cmd.SetContext(client.NewContext(cmd.Context(), c))

			return nil
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.StringVar(&caCert, "ca-cert", "", "path to a PEM file holding the certificate authority to check the server against")

	if info, ok := debug.ReadBuildInfo(); ok {
		cmd.Version = info.Main.Version
	}

	cmd.AddCommand(
		serve.Command(),
		workload.Command(),
		volume.Command(),
		secret.Command(),
		variable.Command(),
		admin.Command(),
		dev.Command(),
	)

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
