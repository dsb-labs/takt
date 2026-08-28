// Package set provides the CLI endpoint to the "secret set" command.
package set

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// The largest value that will be read.
//
// A secret is a credential, not a payload, and the server refuses a request body over
// a megabyte anyway. Reading a bounded amount means a caller who redirected the wrong
// file is told so rather than filling memory.
const maxValue = 1 << 20

// Command returns the "secret set" command used to store a secret's value.
func Command() *cobra.Command {
	var address string
	var file string
	var labels map[string]string

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Set a secret's value",
		Long: "Set a secret's value, reading it from a file or from standard input.\n\n" +
			"There is deliberately no flag that takes the value. Arguments are visible to\n" +
			"anything that can list processes on the host, and to the shell history, so a\n" +
			"flag would undo the feature for whoever used it.\n\n" +
			"Setting a secret to the value it already holds does nothing, so a script that\n" +
			"sets every secret on every run does not restart the workloads reading them. A\n" +
			"value that did change replaces those workloads, and reaches them as they\n" +
			"start.\n\n" +
			"The value is taken exactly as given, including any trailing newline. Use\n" +
			"--from-file to read a file, or pipe the value in:\n\n" +
			"  orca secret set db-password --from-file ./password\n" +
			"  printf %s hunter2 | orca secret set db-password\n\n" +
			"Labels are replaced, not merged, the way a workload manifest replaces a\n" +
			"workload's. Setting a value without --label removes the labels the secret\n" +
			"had. Labelling one is not a rotation: the revision stays put and nothing\n" +
			"reading the secret is replaced.\n\n" +
			"A label is as readable as the secret's name. The value is not, and a label\n" +
			"is no place to put one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := read(cmd, file)
			if err != nil {
				return err
			}

			c, err := client.New(address)
			if err != nil {
				return err
			}

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
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
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
