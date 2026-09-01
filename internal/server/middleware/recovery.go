package middleware

import (
	"log/slog"
	"net/http"
)

// Recovery returns middleware that catches panics from downstream handlers, logs
// them, and writes a 500 response.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					logger.With("panic", rv, "path", r.URL.Path).Error("handler panicked")
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
