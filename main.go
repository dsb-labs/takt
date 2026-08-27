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
	"github.com/dsb-labs/orca/cmd/secret"
	"github.com/dsb-labs/orca/cmd/serve"
	"github.com/dsb-labs/orca/cmd/variable"
	"github.com/dsb-labs/orca/cmd/volume"
	"github.com/dsb-labs/orca/cmd/workload"
	"github.com/dsb-labs/orca/internal/server/driver/exec"
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

	cmd := &cobra.Command{
		Use:          "orca",
		Short:        "A single-node workload orchestrator",
		SilenceUsage: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

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
	)

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
