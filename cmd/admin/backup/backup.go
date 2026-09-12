// Package backup provides the CLI endpoint to the "admin backup" command.
package backup

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/takt/pkg/client"
)

// The Result type is what the command reports about the file it wrote.
type Result struct {
	// Where the backup was written.
	Path string
	// How large it is.
	Bytes int64
}

//go:embed usage.txt
var usage string

// Command returns the "admin backup" command used to write a backup of the node.
func Command() *cobra.Command {
	var includeKeys bool

	cmd := &cobra.Command{
		Use:   "backup <destination>",
		Short: "Write a backup of the node to a file",
		Long:  usage,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client.FromContext(cmd.Context())

			// Exclusive, so a destination that is already there is refused rather
			// than replaced. A backup is written to somewhere backups are kept, and
			// overwriting one of those is not something to do without being asked.
			//
			// Readable only by the owner: the archive holds every workload's
			// specification, environment included, and the key when it was asked for.
			f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			switch {
			case errors.Is(err, os.ErrExist):
				return fmt.Errorf("%s already exists, and a backup does not replace one", args[0])
			case err != nil:
				return fmt.Errorf("failed to create backup file: %w", err)
			}
			defer f.Close()

			var options []client.BackupOption
			if includeKeys {
				options = append(options, client.WithKeys())
			}

			if err = c.Backup(cmd.Context(), f, options...); err != nil {
				err = fmt.Errorf("failed to write backup: %w", err)

				// The file has already been created, and what is in it is a partial
				// archive at best. Left behind it looks like a backup, so a request
				// that failed takes it with it.
				//
				// A removal that itself fails is joined onto the original rather than
				// replacing it. Which request failed is the more useful half, and the
				// file left behind is the half that needs acting on.
				if removeErr := os.Remove(args[0]); removeErr != nil {
					return errors.Join(err, fmt.Errorf("failed to remove %s: %w", args[0], removeErr))
				}

				return err
			}

			info, err := f.Stat()
			if err != nil {
				return fmt.Errorf("failed to read backup file: %w", err)
			}

			// What the backup does not cover, on stderr so that stdout stays a
			// document something else can read. Both are things an operator finds out
			// too late otherwise.
			out := cmd.ErrOrStderr()

			fmt.Fprintln(out, "Volume data is not in this backup. Run \"takt volume list\" for each volume's path.")

			if !includeKeys {
				fmt.Fprintln(out, "The keyring is not in this backup. It needs a backup of its own,")
				fmt.Fprintln(out, "or nothing here can be decrypted.")
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(Result{Path: args[0], Bytes: info.Size()})
		},
	}

	cmd.Flags().BoolVar(&includeKeys, "include-keys", false, "put the keyring in the archive, which makes it key material")

	return cmd
}
