package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Guard returns middleware that refuses a request whose Host or Origin names
// something other than this server.
//
// Reaching this API is enough to run code on the host, so the default is to listen on
// loopback. That is not the boundary it appears to be. A browser will happily send a
// request to 127.0.0.1 on behalf of a page the operator merely visited: the attacker
// serves that page from a name they control, points the name at 127.0.0.1, and the
// browser treats what follows as same-origin — so nothing about the connection being
// local says anything about who asked for it.
//
// The check is what closes that. An attack of this shape needs a name, because the
// attacker has to control what the name resolves to, so a Host naming an address
// rather than a name cannot be one: a page served from an address literal is a page
// served from this server. A name has to be one the operator named, which is what a
// reverse proxy in front of takt needs.
//
// The Origin is checked by the same rule. The web UI is served from this server and so
// shares its origin, so an Origin naming somewhere else is a page acting on its own
// behalf rather than a client acting on the operator's. Refusing it here matters
// because a browser sends some cross-origin requests whether or not it is allowed to
// read the answer, and creating a volume does not need an answer to have happened.
func Guard(logger *slog.Logger, permitted []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(permitted))
	for _, host := range permitted {
		allowed[strings.ToLower(host)] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !permittedHost(hostname(r.Host), allowed) {
				logger.With("host", r.Host, "path", r.URL.Path).
					Debug("refused a request naming an unexpected host")
				writeError(w, http.StatusMisdirectedRequest, "request names an unexpected host")

				return
			}

			// Absent on everything but a browser, which is the point: a client that
			// sends none is not one a page is driving.
			if origin := r.Header.Get("Origin"); origin != "" && !permittedOrigin(origin, allowed) {
				logger.With("origin", origin, "path", r.URL.Path).
					Debug("refused a request from an unexpected origin")
				writeError(w, http.StatusForbidden, "request comes from an unexpected origin")

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// permittedHost reports whether a request may name the given host.
//
// An address literal is always allowed. Reaching takt by one means the caller already
// knew where it was, and a page served from an address is a page served from this
// server — where a name is something an attacker can point wherever they like.
func permittedHost(host string, allowed map[string]struct{}) bool {
	if host == "" {
		return false
	}

	if net.ParseIP(host) != nil {
		return true
	}

	if strings.EqualFold(host, "localhost") {
		return true
	}

	_, ok := allowed[strings.ToLower(host)]

	return ok
}

// permittedOrigin reports whether a request may come from the given origin.
//
// An origin takt cannot parse is refused, as is the opaque "null" a sandboxed page
// sends: neither names somewhere this API is served from.
func permittedOrigin(origin string, allowed map[string]struct{}) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}

	return permittedHost(parsed.Hostname(), allowed)
}

// hostname returns the name part of a Host header, which carries a port when the
// server is not on the scheme's default one.
func hostname(host string) string {
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		return parsed
	}

	// No port to split, so the whole value is the name. A bracketed IPv6 literal
	// still has to have its brackets taken off before it parses as an address.
	return strings.Trim(host, "[]")
}
