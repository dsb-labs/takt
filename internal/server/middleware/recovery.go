package middleware

import (
	"log/slog"
	"net/http"
)

// Recovery returns middleware that catches panics from downstream handlers, logs
// them, and writes a 500 response.
//
// A handler that panics with http.ErrAbortHandler is asking the server to close
// the connection without finishing the response, which is how a stream that
// failed after its status went out tells the client so. That one passes through
// to the server, which knows not to log it.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rv := recover()
				switch {
				case rv == nil:
				case rv == http.ErrAbortHandler:
					panic(rv)
				default:
					logger.With("panic", rv, "path", r.URL.Path).Error("handler panicked")
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
