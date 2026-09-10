// Package login provides the CLI endpoint to the "auth login" command.
package login

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/dsb-labs/takt/pkg/cli"
	"github.com/dsb-labs/takt/pkg/client"
)

// The Result type is what the command prints. The credential itself is not
// here: it went into the config file, which is the point of logging in.
type Result struct {
	// The principal the minted token is bound to.
	Principal string
	// The time the token stops authenticating. Log in again after it.
	ExpiresAt time.Time
}

// How long the browser has to complete the flow before the command gives up.
const loginTimeout = 5 * time.Minute

// Command returns the "auth login" command used to exchange an OIDC identity
// for a short-lived token.
func Command() *cobra.Command {
	var port int
	var scopes []string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in with OIDC and store the minted token",
		Long: "Log in through the server's OIDC issuer and store the minted token in the\n" +
			"config file.\n\n" +
			"The command asks the server who its issuer is, runs the authorization\n" +
			"code flow against a loopback callback, and hands the code to the server\n" +
			"to exchange for a short-lived client token — the exchange is what needs\n" +
			"the issuer's client secret, so the secret stays on the server. Open the\n" +
			"printed URL in a browser when one does not open on its own. The issuer\n" +
			"must permit the redirect URI http://127.0.0.1:8250/oidc/callback, or\n" +
			"the one --callback-port names.\n\n" +
			"The token lands in the config file rather than on stdout, because a\n" +
			"child process cannot set an environment variable in its parent shell\n" +
			"and every login ending in copy-paste ceremony would be the alternative.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()

			c := client.FromContext(ctx)

			discovered, err := c.GetOIDC(ctx)
			if err != nil {
				return fmt.Errorf("failed to read the server's oidc configuration: %w", err)
			}

			provider, err := oidc.NewProvider(ctx, discovered.Issuer)
			if err != nil {
				return fmt.Errorf("failed to discover the oidc issuer: %w", err)
			}

			// The listener is bound before the URL is printed, so the issuer
			// cannot send the browser back to a port nothing answers on.
			listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return fmt.Errorf("failed to listen for the callback: %w", err)
			}
			defer listener.Close()

			flow := &oauth2.Config{
				ClientID:    discovered.ClientID,
				Endpoint:    provider.Endpoint(),
				RedirectURL: fmt.Sprintf("http://%s/oidc/callback", listener.Addr()),
				Scopes:      scopes,
			}

			nonce := make([]byte, 16)
			if _, err = rand.Read(nonce); err != nil {
				return fmt.Errorf("failed to generate state: %w", err)
			}

			state := hex.EncodeToString(nonce)
			verifier := oauth2.GenerateVerifier()

			fmt.Fprintf(cmd.ErrOrStderr(), "Open this URL in your browser to log in:\n\n  %s\n\n",
				flow.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)))

			code, err := waitForCode(ctx, listener, state)
			if err != nil {
				return err
			}

			// The server performs the exchange, because the exchange is what
			// needs the issuer's client secret and the secret never reaches
			// this command.
			login, err := c.LoginCode(ctx, code, verifier, flow.RedirectURL)
			if err != nil {
				return fmt.Errorf("failed to log in: %w", err)
			}

			configFlag, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}

			if err = storeCredential(configFlag, login.Credential); err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")

			return enc.Encode(Result{Principal: login.Principal, ExpiresAt: login.ExpiresAt})
		},
	}

	flags := cmd.Flags()
	flags.IntVar(&port, "callback-port", 8250, "loopback port the issuer sends the browser back to")
	flags.StringSliceVar(&scopes, "scopes", []string{"openid", "email", "profile"},
		"scopes to request from the issuer")

	return cmd
}

// waitForCode serves the loopback callback until the issuer sends the browser
// to it with an authorization code, and returns that code.
func waitForCode(ctx context.Context, listener net.Listener, state string) (string, error) {
	codes := make(chan string, 1)

	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/oidc/callback" {
				http.NotFound(w, r)

				return
			}

			query := r.URL.Query()
			if query.Get("state") != state || query.Get("code") == "" {
				http.Error(w, "the callback does not match the login this command started", http.StatusUnauthorized)

				return
			}

			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintln(w, "Logged in. You can close this tab and return to the terminal.")

			codes <- query.Get("code")
		}),
	}

	var code string

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		return nil
	})
	g.Go(func() error {
		defer server.Close()

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for the browser: %w", ctx.Err())
		case code = <-codes:
			return nil
		}
	})

	if err := g.Wait(); err != nil {
		return "", err
	}

	return code, nil
}

// storeCredential writes the minted token into the config file, keeping the
// settings already there.
func storeCredential(configFlag, credential string) error {
	path, err := cli.Path(configFlag)
	if err != nil {
		return err
	}

	settings, err := cli.Load(path)
	if err != nil {
		return err
	}

	settings.Token = credential

	return cli.Write(path, settings)
}
