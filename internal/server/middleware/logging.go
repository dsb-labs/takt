package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logging returns middleware that records every request's method, path, status
// and duration at debug level.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &recordingResponseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r)

			logger.With(
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration", time.Since(start),
			).Debug("http request")
		})
	}
}

type recordingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *recordingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap returns the writer underneath, which is how http.ResponseController reaches
// what this wrapper does not implement itself.
//
// Embedding the interface promotes only its three methods, so a handler that flushes a
// streaming response would otherwise be told the writer cannot flush. That failure is
// silent: the output still arrives, in whatever chunks the connection's buffer happens
// to produce, which for a followed log read means nothing arrives until the workload
// has said four kilobytes' worth.
func (w *recordingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
