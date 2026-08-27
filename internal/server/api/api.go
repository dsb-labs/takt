// Package api provides the HTTP API surface of the orca server.
//
// The routes, request decoding and response marshalling are generated from
// api/openapi.yaml; this package implements the generated interface and holds the
// middleware that wraps it.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
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
		*SecretAPI
		*VariableAPI
		*SystemAPI
		*AdminAPI
	}

	// The Config type contains fields used to construct an API.
	Config struct {
		// The endpoints serving workloads.
		Workloads *WorkloadAPI
		// The endpoints serving volumes.
		Volumes *VolumeAPI
		// The endpoints serving secrets.
		Secrets *SecretAPI
		// The endpoints serving variables.
		Variables *VariableAPI
		// The endpoints describing the server itself.
		System *SystemAPI
		// The endpoints acting on the node itself.
		Admin *AdminAPI
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
		SecretAPI:   config.Secrets,
		VariableAPI: config.Variables,
		SystemAPI:   config.System,
		AdminAPI:    config.Admin,
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

// Wrap returns handler with the middleware the server puts in front of it.
//
// The order is what this exists to hold still. Each entry wraps what came before, so
// the list runs innermost first and a request meets it bottom to top: a body is bounded
// before anything reads it, the host is checked before a handler runs, and Recovery sits
// directly around the handler where a panic is actually likely to come from.
//
// Recovery being innermost is deliberate rather than incidental. A panic that unwound
// past Logging would leave no record of the request that caused it, and the answer to
// "which request killed this" is the reason the log line is worth having at all.
func Wrap(handler http.Handler, logger *slog.Logger, hosts []string) http.Handler {
	for _, middleware := range []func(http.Handler) http.Handler{
		Recovery(logger),
		Logging(logger),
		// Ahead of anything that reaches a handler. Reaching this API is enough to run
		// code on the host, and listening on loopback does not establish that the
		// operator is who asked — a browser sends a request there on behalf of whatever
		// page it was told to.
		Guard(logger, hosts),
		RequireJSON,
		Limit,
	} {
		handler = middleware(handler)
	}

	return handler
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
// reverse proxy in front of orca needs.
//
// The Origin is checked by the same rule. Nothing that speaks to this API from a
// browser exists, so an Origin naming somewhere else is a page acting on its own
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

// permittedHost reports whether a request may name the given host.
//
// An address literal is always allowed. Reaching orca by one means the caller already
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
// An origin orca cannot parse is refused, as is the opaque "null" a sandboxed page
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

// writeError writes an error response in the shape every other failure uses, so that
// a rejection before the handler reads the same way to a client as one after it.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(ErrorResponse{Status: status, Message: message})
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

// The key the connection's own ResponseWriter is stored under.
type connectionKey struct{}

// Stream returns middleware that keeps the connection's own ResponseWriter within reach
// of the handlers.
//
// This has to sit outside every other wrapper, including the telemetry one. Each of them
// passes writes through and none of them carries SetWriteDeadline, and unlike a flush
// there is no way to reach past them for it: http.ResponseController stops at the first
// wrapper that cannot unwrap.
//
// A handler that ignores this sees no difference. The one that does not is the followed
// log read, which is open for as long as a workload runs and so cannot live under a
// deadline meant for a request that answers and stops.
func Stream(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), connectionKey{}, w)))
	})
}

// Connection returns the ResponseWriter Stream stored, or nil when nothing did. It is
// the counterpart to Stream, and the only way to read what it put there.
//
// Nil is an ordinary answer rather than a failure. A handler served without the
// middleware still writes its response, and only loses the control over the connection
// that it would rather have had.
func Connection(ctx context.Context) http.ResponseWriter {
	w, _ := ctx.Value(connectionKey{}).(http.ResponseWriter)

	return w
}
