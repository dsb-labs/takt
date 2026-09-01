package middleware

import (
	"mime"
	"net/http"
	"strings"
)

// RequireJSON returns middleware that refuses a request carrying a body that does not
// declare itself as JSON.
//
// The API reads JSON, so this is what the server accepts anyway. It is enforced rather
// than assumed because of which requests a browser will send across origins without
// asking first: a form-encoded or plain-text body is one of them, where a JSON one is
// not. Requiring the content type means a request that changes something has to be one
// a browser would have had to ask permission for.
func RequireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request with nothing to decode has no content type to check. Length is
		// unset on a chunked body, so the method is what says a body was meant.
		//
		// Every write this API serves carries a JSON object, including the ones with
		// nothing to say: a rekey sends an empty one rather than no body at all. So a
		// write arriving without one has nothing this can let through, and a bodyless
		// POST is exactly what a browser may send across origins without asking
		// permission first.
		if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodPatch {
			next.ServeHTTP(w, r)

			return
		}

		// The header carries parameters as well as the type, so only the media type
		// is compared. A malformed value is refused rather than guessed at.
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(media, "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "request body must be application/json")

			return
		}

		next.ServeHTTP(w, r)
	})
}
