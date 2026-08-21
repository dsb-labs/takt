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

	"github.com/dsb-labs/orca/internal/generated/api"
)

type (
	// The ErrorResponse type is the JSON shape returned for error responses, and
	// implements the error interface so that clients can return it directly.
	ErrorResponse struct {
		// The HTTP status code. Set when writing the response; not sent on the wire.
		Status int `json:"-"`
		// The human-readable error message.
		Message string `json:"error"`
	}

	// The API type is the whole HTTP surface, assembled from the types serving each
	// resource.
	//
	// The wire format describes one API, so the generated code describes one
	// interface covering every operation in it. Each resource still gets a type of
	// its own, holding only the service it needs, and embedding them here is what
	// makes the set of them satisfy that interface. None of them has to know the
	// others exist.
	API struct {
		*WorkloadAPI
		*VolumeAPI
	}

	// The Config type contains fields used to construct an API.
	Config struct {
		// The endpoints serving workloads.
		Workloads *WorkloadAPI
		// The endpoints serving volumes.
		Volumes *VolumeAPI
	}
)

// Error returns the error message.
func (e ErrorResponse) Error() string {
	return e.Message
}

// New returns an API serving each of the given resources.
func New(config Config) *API {
	return &API{
		WorkloadAPI: config.Workloads,
		VolumeAPI:   config.Volumes,
	}
}

// Register the HTTP endpoints onto the given http.ServeMux.
//
// The routes themselves come from the generated handler, which is mounted onto the
// caller's mux rather than one of its own so that the server keeps ownership of
// routing and can wrap the whole surface in its own middleware.
func (a *API) Register(mux *http.ServeMux) {
	api.HandlerWithOptions(api.NewStrictHandler(a, nil), api.StdHTTPServerOptions{
		BaseRouter: mux,
	})
}

// The largest request body the server will read. A workload specification is a
// small document — a manifest an operator wrote by hand — so this is generous for
// anything legitimate while refusing to read an endless upload into memory.
const maxRequestBody = 1 << 20

// Limit returns middleware that refuses to read more than maxRequestBody from a
// request.
//
// Without it a request body is read until it ends, which a client is under no
// obligation to do: an upload that never finishes is memory the server keeps
// accepting. The limit is applied to the body rather than checked against
// Content-Length, since a chunked request declares no length to check.
func Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}

		next.ServeHTTP(w, r)
	})
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
