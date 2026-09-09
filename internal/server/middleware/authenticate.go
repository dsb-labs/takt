package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/service"
)

type (
	// The Authenticator interface describes how a presented credential
	// becomes an identity.
	Authenticator interface {
		// Authenticate should resolve a credential to the identity it
		// proves, reporting service.ErrInvalidCredential for one that does
		// not authenticate.
		Authenticate(ctx context.Context, credential string) (auth.Identity, error)
	}

	// The key the caller's identity is stored under.
	identityKey struct{}
)

// Authenticate returns middleware that resolves the request's credential to
// an identity and stores it in the request context.
//
// The bearer header is read first, then the session cookie. A presented
// credential that does not authenticate is refused here, so a caller with a
// revoked token learns so on every route the same way. A request presenting
// nothing proceeds as the anonymous identity: whether anonymity is enough is
// each operation's declared requirement, not this middleware's, and the
// health probes and the UI's static files depend on it not deciding.
//
// A nil authenticator means the configuration carries no [auth] block. Every
// request then proceeds as the disabled identity, which passes every
// requirement — the network boundary stays the whole check, exactly as it
// was before the layer existed.
func Authenticate(authenticator Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authenticator == nil {
				next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), auth.Identity{Disabled: true})))

				return
			}

			credential := bearerCredential(r)
			if credential == "" {
				if cookie, err := r.Cookie(auth.SessionCookie); err == nil {
					credential = cookie.Value
				}
			}

			if credential == "" {
				next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), auth.Identity{})))

				return
			}

			identity, err := authenticator.Authenticate(r.Context(), credential)
			switch {
			case errors.Is(err, service.ErrInvalidCredential):
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "invalid credential")

				return
			case err != nil:
				writeError(w, http.StatusInternalServerError, "failed to authenticate request")

				return
			}

			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity)))
		})
	}
}

// CallerIdentity returns the identity Authenticate stored, or the anonymous
// identity when nothing did. It is the counterpart to Authenticate, and the
// only way to read what it put there.
func CallerIdentity(ctx context.Context) auth.Identity {
	identity, _ := ctx.Value(identityKey{}).(auth.Identity)

	return identity
}

// withIdentity stores the identity for CallerIdentity to read.
func withIdentity(ctx context.Context, identity auth.Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// bearerCredential reads the credential off the Authorization header, which
// is empty when the header is absent or carries another scheme.
func bearerCredential(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}

	scheme, credential, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}

	return strings.TrimSpace(credential)
}
