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

	"github.com/dsb-labs/orca/cmd/apply"
	delcmd "github.com/dsb-labs/orca/cmd/delete"
	"github.com/dsb-labs/orca/cmd/get"
	"github.com/dsb-labs/orca/cmd/list"
	"github.com/dsb-labs/orca/cmd/logs"
	"github.com/dsb-labs/orca/cmd/serve"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
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
		apply.Command(),
		list.Command(),
		get.Command(),
		delcmd.Command(),
		logs.Command(),
	)

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
