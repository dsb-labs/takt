package middleware

import (
	"net/http"
)

// Headers returns middleware that sets the response headers a browser reads as
// policy, on every response the server writes.
//
// The content security policy is what the web UI needs and nothing more: scripts
// and styles from this origin, with inline styles allowed because the UI colours
// log output and lays out graphs through style bindings. Nothing is loaded from
// anywhere else, so every other source is the origin. Framing is refused, since
// nothing legitimate embeds the UI and a page that did could lay its own
// controls over it. The type sniffing header stops a browser reading a JSON
// response as something it is not.
//
// Strict transport security is sent only when the server is reached over TLS,
// since a browser told to insist on TLS for a name it reached over plain HTTP
// would refuse the name from then on.
func Headers(tls bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := w.Header()
			header.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; "+
					"base-uri 'self'; form-action 'self'")
			header.Set("X-Content-Type-Options", "nosniff")
			header.Set("X-Frame-Options", "DENY")
			header.Set("Referrer-Policy", "same-origin")

			if tls {
				header.Set("Strict-Transport-Security", "max-age=31536000")
			}

			next.ServeHTTP(w, r)
		})
	}
}
