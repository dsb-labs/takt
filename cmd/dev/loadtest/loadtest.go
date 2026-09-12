// Package loadtest provides the CLI endpoint to the "dev loadtest" command.
package loadtest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/internal/loadtest"
	"github.com/dsb-labs/takt/pkg/client"
)

//go:embed usage.txt
var usage string

// Command returns the "dev loadtest" command used to drive a server through a
// scenario.
func Command() *cobra.Command {
	var dataDir string
	var prefix string
	var keep bool

	cmd := &cobra.Command{
		Use:   "loadtest <scenario>",
		Short: "Drive a server through a load test scenario",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("failed to open scenario: %w", err)
			}
			defer f.Close()

			scenario, err := loadtest.ParseScenario(f)
			if err != nil {
				return err
			}

			c := client.FromContext(cmd.Context())

			report, err := loadtest.Run(cmd.Context(), loadtest.Config{
				Scenario: scenario,
				Client:   c,
				Prefix:   prefix,
				DataDir:  dataDir,
				Keep:     keep,
			})
			if err != nil {
				return fmt.Errorf("failed to run the scenario: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			if err = enc.Encode(report); err != nil {
				return err
			}

			// A run that found something exits non-zero, so this is usable as a check
			// rather than only as a measurement. What it found is in the report, and
			// repeating it here would be saying it twice.
			if report.Failed() {
				return fmt.Errorf("the scenario found problems")
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&dataDir, "data-dir", "", "the server's data directory, to also report what a run left on disk")
	flags.StringVar(&prefix, "prefix", defaultPrefix(), "what every name the run creates begins with")
	flags.BoolVar(&keep, "keep", false, "leave the fleet in place instead of removing it")

	return cmd
}

// defaultPrefix returns a prefix unique to this run, so that two load tests against
// one server do not tear down each other's workloads.
func defaultPrefix() string {
	return fmt.Sprintf("load%d", time.Now().Unix()%100000)
}
