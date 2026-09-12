// Package set provides the CLI endpoint to the "variable set" command.
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

// The largest value that will be read from a file or from standard input.
//
// A variable is configuration, not a payload, and the server refuses a request body
// over a megabyte anyway. Reading a bounded amount means a caller who redirected the
// wrong file is told so rather than filling memory.
const maxValue = 1 << 20

//go:embed usage.txt
var usage string

// Command returns the "variable set" command used to store a variable's value.
func Command() *cobra.Command {
	var file string
	var labels map[string]string

	cmd := &cobra.Command{
		Use:   "set <name> [value]",
		Short: "Set a variable's value",
		Long:  usage,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := read(cmd, args, file)
			if err != nil {
				return err
			}

			c := client.FromContext(cmd.Context())

			variable, _, err := c.SetVariable(cmd.Context(), args[0], value, labels)
			if err != nil {
				return fmt.Errorf("failed to set variable: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(variable)
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&file, "from-file", "f", "", "read the value from this file rather than the argument or standard input")
	flags.StringToStringVarP(&labels, "label", "l", nil,
		"a key=value label to attach, repeatable. The labels given replace the ones stored")

	return cmd
}

// read returns the value to store, from the argument, the named file, or standard
// input.
//
// An argument and --from-file together is refused rather than one silently winning.
// The caller named two sources for one value, and guessing which was meant is how a
// variable ends up holding the wrong thing.
func read(cmd *cobra.Command, args []string, file string) (string, error) {
	switch {
	case len(args) > 1 && file != "":
		return "", fmt.Errorf("the value was given as an argument and as --from-file: use one")
	case len(args) > 1:
		return args[1], nil
	case file != "":
		return readFile(file)
	}

	value, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxValue))
	if err != nil {
		return "", fmt.Errorf("failed to read the value: %w", err)
	}

	return string(value), nil
}

func readFile(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("failed to open the value: %w", err)
	}
	defer f.Close()

	value, err := io.ReadAll(io.LimitReader(f, maxValue))
	if err != nil {
		return "", fmt.Errorf("failed to read the value: %w", err)
	}

	return string(value), nil
}
