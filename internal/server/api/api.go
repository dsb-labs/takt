// Package api provides the HTTP API surface of the orca server.
//
// The routes, request decoding and response marshalling are generated from
// api/openapi.yaml; this package implements the generated interface and holds the
// middleware that wraps it.
package api

import (
	"log/slog"
	"net/http"
	"time"
)

// The ErrorResponse type is the JSON shape returned for error responses, and
// implements the error interface so that clients can return it directly.
type ErrorResponse struct {
	// The HTTP status code. Set when writing the response; not sent on the wire.
	Status int `json:"-"`
	// The human-readable error message.
	Message string `json:"error"`
}

// Error returns the error message.
func (e ErrorResponse) Error() string {
	return e.Message
}

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

type recordingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *recordingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
