// Package backup provides the CLI endpoint to the "admin backup" command.
package backup

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsb-labs/orca/pkg/client"
)

// The Result type is what the command reports about the file it wrote.
type Result struct {
	// Where the backup was written.
	Path string
	// How large it is.
	Bytes int64
}

// Command returns the "admin backup" command used to write a backup of the node.
func Command() *cobra.Command {
	var address string
	var includeKey bool

	cmd := &cobra.Command{
		Use:   "backup <destination>",
		Short: "Write a backup of the node to a file",
		Long: "Write a backup of the node to a file.\n\n" +
			"The server takes a consistent snapshot of its database while it keeps\n" +
			"running, and writes it to the destination as a zip archive. Copying\n" +
			"state.db by hand does not do this: the database runs in write-ahead\n" +
			"logging mode, so what is committed at any moment is spread across three\n" +
			"files and a copy of one of them is stale or torn.\n\n" +
			"Volume data is not in the archive. Run \"orca volume list\" for the path\n" +
			"of each volume on the host and back those up separately.\n\n" +
			"The encryption key is not in the archive either, unless --include-key is\n" +
			"passed. A database without its key decrypts nothing, which is what makes\n" +
			"a copy of it safe to keep somewhere a key would not be.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.New(address)
			if err != nil {
				return err
			}

			// Exclusive, so a destination that is already there is refused rather
			// than replaced. A backup is written to somewhere backups are kept, and
			// overwriting one of those is not something to do without being asked.
			//
			// Readable only by the owner: the archive holds every workload's
			// specification, environment included, and the key when it was asked for.
			f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("failed to create backup file: %w", err)
			}
			defer f.Close()

			var options []client.BackupOption
			if includeKey {
				options = append(options, client.WithKey())
			}

			if err = c.Backup(cmd.Context(), f, options...); err != nil {
				// The file has already been created, and what is in it is a partial
				// archive at best. Left behind it looks like a backup, so a request
				// that failed takes it with it.
				if removeErr := os.Remove(args[0]); removeErr != nil {
					return fmt.Errorf("failed to write backup, and failed to remove %s: %w", args[0], removeErr)
				}

				return fmt.Errorf("failed to write backup: %w", err)
			}

			info, err := f.Stat()
			if err != nil {
				return fmt.Errorf("failed to read backup file: %w", err)
			}

			// What the backup does not cover, on stderr so that stdout stays a
			// document something else can read. Both are things an operator finds out
			// too late otherwise.
			out := cmd.ErrOrStderr()

			fmt.Fprintln(out, "Volume data is not in this backup. Run \"orca volume list\" for each volume's path.")

			if !includeKey {
				fmt.Fprintln(out, "The encryption key is not in this backup. It needs a backup of its own,")
				fmt.Fprintln(out, "or nothing here can be decrypted.")
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(Result{Path: args[0], Bytes: info.Size()})
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&address, "address", "a", "http://localhost:7373", "URL of the orca server")
	flags.BoolVar(&includeKey, "include-key", false, "put the secret encryption key in the archive, which makes it key material")

	return cmd
}
