package apply

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

// The largest secret value read from a file or a prompt, matching what "secret
// set" accepts.
const maxSecret = 1 << 20

// setSecrets sets every declared secret that does not exist yet, from the
// --secret flags first and then by prompting, and reports every one it could
// not set at once.
//
// Only a missing secret is set. Supplying one is a per-invocation act, never
// recorded in the score, and setting a secret that exists would bump its
// revision and redeploy every reader on every apply.
func setSecrets(cmd *cobra.Command, c *client.Client, rendered score.Rendered, missing []score.Node, flags []string, noInput bool) error {
	supplied, err := parseSecretFlags(flags, rendered.Secrets)
	if err != nil {
		return err
	}

	var names []string
	for _, node := range missing {
		if node.Kind == score.KindSecret {
			names = append(names, node.Name)
		}
	}

	for name := range supplied {
		if !slices.Contains(names, name) {
			fmt.Fprintf(cmd.ErrOrStderr(), "secret %s is already set and was not replaced\n", name)
		}
	}

	var unset []string
	for _, name := range names {
		value, ok, err := secretValue(cmd, name, supplied, noInput)
		if err != nil {
			return err
		}

		if !ok {
			unset = append(unset, name)
			continue
		}

		if err = setSecret(cmd.Context(), c, name, value); err != nil {
			return err
		}
	}

	if len(unset) > 0 {
		return fmt.Errorf("%w: secret %s", score.ErrMissing, strings.Join(unset, ", secret "))
	}

	return nil
}

// secretValue returns the value for a missing secret: the file a flag named, or
// what the operator types at a prompt. The second result is false when neither
// can supply one.
func secretValue(cmd *cobra.Command, name string, supplied map[string]string, noInput bool) ([]byte, bool, error) {
	if path, ok := supplied[name]; ok {
		value, err := readSecretFile(path)
		if err != nil {
			return nil, false, fmt.Errorf("failed to read secret %s: %w", name, err)
		}

		return value, true, nil
	}

	if noInput {
		return nil, false, nil
	}

	f, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil, false, nil
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "secret %s: ", name)

	value, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return nil, false, fmt.Errorf("failed to read secret %s: %w", name, err)
	}

	return value, true, nil
}

// parseSecretFlags reads every --secret flag as name=@path, refusing a value
// given inline and a name the score does not declare.
//
// A value on the command line is visible to anything that can list processes
// and lands in shell history, which is the reason "secret set" takes no value
// argument either. An undeclared name is refused as a typo, since a score
// declares every secret its workloads read.
func parseSecretFlags(flags []string, declared []string) (map[string]string, error) {
	supplied := make(map[string]string, len(flags))
	for _, flag := range flags {
		name, source, ok := strings.Cut(flag, "=")
		switch {
		case !ok || name == "":
			return nil, fmt.Errorf("invalid --secret %q: expected name=@path", flag)
		case !strings.HasPrefix(source, "@") || len(source) == 1:
			return nil, fmt.Errorf("invalid --secret %q: a value must be read from a file, as name=@path", flag)
		case !slices.Contains(declared, name):
			return nil, fmt.Errorf("invalid --secret %q: the score does not declare secret %s", flag, name)
		}

		supplied[name] = source[1:]
	}

	return supplied, nil
}

func readSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	value, err := io.ReadAll(io.LimitReader(f, maxSecret))
	if err != nil {
		return nil, err
	}

	return value, nil
}

func setSecret(ctx context.Context, c *client.Client, name string, value []byte) error {
	if _, _, err := c.SetSecret(ctx, manifest.Secret{Name: name, Value: value}); err != nil {
		return fmt.Errorf("failed to set secret %s: %w", name, err)
	}

	return nil
}
