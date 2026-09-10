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
			identity, ok := resolveIdentity(w, r, authenticator)
			if !ok {
				return
			}

			serveIdentified(w, r, next, identity)
		})
	}
}

// resolveIdentity turns the request's credential into the identity it proves.
// The second return value is false only when a refusal has already been
// written, so the caller stops rather than serving the request.
func resolveIdentity(w http.ResponseWriter, r *http.Request, authenticator Authenticator) (auth.Identity, bool) {
	if authenticator == nil {
		return auth.Identity{Disabled: true}, true
	}

	credential := bearerCredential(r)
	if credential == "" {
		if cookie, err := r.Cookie(auth.SessionCookie); err == nil {
			credential = cookie.Value
		}
	}

	if credential == "" {
		return auth.Identity{}, true
	}

	identity, err := authenticator.Authenticate(r.Context(), credential)
	switch {
	case errors.Is(err, service.ErrInvalidCredential):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid credential")

		return auth.Identity{}, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to authenticate request")

		return auth.Identity{}, false
	}

	return identity, true
}

// serveIdentified runs next with the identity in the request context, then
// copies the route the router matched back onto the request the caller holds.
//
// otelhttp records http.route from r.Pattern on the very request it passed
// down, read once next returns. This middleware hands next a WithContext copy,
// so http.ServeMux records the match on that copy rather than on the request
// otelhttp holds. Copying the pattern back is what keeps http.route on the
// server's spans and metrics. It is sound because nothing between otelhttp and
// this middleware replaces the request, so the r here is the one otelhttp reads.
func serveIdentified(w http.ResponseWriter, r *http.Request, next http.Handler, identity auth.Identity) {
	identified := r.WithContext(withIdentity(r.Context(), identity))
	next.ServeHTTP(w, identified)
	r.Pattern = identified.Pattern
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
