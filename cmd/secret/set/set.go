// Package set provides the CLI endpoint to the "secret set" command.
package set

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// The largest value that will be read.
//
// A secret is a credential, not a payload, and the server refuses a request body over
// a megabyte anyway. Reading a bounded amount means a caller who redirected the wrong
// file is told so rather than filling memory.
const maxValue = 1 << 20

//go:embed usage.txt
var usage string

// Command returns the "secret set" command used to store a secret's value.
func Command() *cobra.Command {
	var file string
	var labels map[string]string

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a secret's value",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := read(cmd, file)
			if err != nil {
				return err
			}

			c := client.FromContext(cmd.Context())

			secret, _, err := c.SetSecret(cmd.Context(), args[0], value, labels)
			if err != nil {
				return fmt.Errorf("failed to set secret: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(secret)
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&file, "from-file", "f", "", "read the value from this file rather than standard input")
	flags.StringToStringVarP(&labels, "label", "l", nil,
		"a key=value label to attach, repeatable. The labels given replace the ones stored")

	return cmd
}

// read returns the value to store, from the named file or from standard input.
func read(cmd *cobra.Command, file string) ([]byte, error) {
	if file == "" {
		value, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxValue))
		if err != nil {
			return nil, fmt.Errorf("failed to read the value: %w", err)
		}

		return value, nil
	}

	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("failed to open the value: %w", err)
	}
	defer f.Close()

	value, err := io.ReadAll(io.LimitReader(f, maxValue))
	if err != nil {
		return nil, fmt.Errorf("failed to read the value: %w", err)
	}

	return value, nil
}
