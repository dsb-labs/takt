// Package api provides the HTTP API surface of the takt server.
//
// The routes, request decoding and response marshalling are generated from
// api/openapi.yaml. This package implements the generated interface. The
// middleware the server wraps around it lives in the middleware package.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/auth"
	"github.com/dsb-labs/takt/internal/server/middleware"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The ErrorResponse type is the JSON shape returned for error responses, and
	// implements the error interface so that clients can return it directly.
	ErrorResponse struct {
		// The HTTP status code. It is set when writing the response and not sent on
		// the wire.
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
		*ServiceAPI
		*SecretAPI
		*VariableAPI
		*NodeAPI
		*SystemAPI
		*AdminAPI
		*AuthAPI
		*ACLAPI
		*TokenAPI
	}

	// The Config type contains fields used to construct an API.
	Config struct {
		// The endpoints serving workloads.
		Workloads *WorkloadAPI
		// The endpoints serving volumes.
		Volumes *VolumeAPI
		// The endpoints serving services.
		Services *ServiceAPI
		// The endpoints serving secrets.
		Secrets *SecretAPI
		// The endpoints serving variables.
		Variables *VariableAPI
		// The endpoints describing the node the server runs on.
		Node *NodeAPI
		// The endpoints describing the server itself.
		System *SystemAPI
		// The endpoints acting on the node itself.
		Admin *AdminAPI
		// The endpoints serving the caller's own authentication.
		Auth *AuthAPI
		// The endpoints serving the policy document.
		ACL *ACLAPI
		// The endpoints managing tokens.
		Tokens *TokenAPI
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
		ServiceAPI:  config.Services,
		SecretAPI:   config.Secrets,
		VariableAPI: config.Variables,
		NodeAPI:     config.Node,
		SystemAPI:   config.System,
		AdminAPI:    config.Admin,
		AuthAPI:     config.Auth,
		ACLAPI:      config.ACL,
		TokenAPI:    config.Tokens,
	}
}

// Register the HTTP endpoints onto the given http.ServeMux.
//
// The routes themselves come from the generated handler, which is mounted onto the
// caller's mux rather than one of its own so that the server keeps ownership of
// routing and can wrap the whole surface in its own middleware.
func (a *API) Register(mux *http.ServeMux) {
	api.HandlerWithOptions(api.NewStrictHandler(a, []api.StrictMiddlewareFunc{authorize}), api.StdHTTPServerOptions{
		BaseRouter: mux,
	})

	// A path under the API prefix that no operation claims is answered in the
	// API's own error shape. Without this it would fall through to whatever else
	// the mux serves at the root — which, with the web UI mounted there, would
	// answer a mistyped API request with an HTML page.
	//
	// Registered once per method rather than for every method at once. A pattern
	// with no method overlaps the UI's GET catch-all at the root without either
	// being the more specific, and the mux refuses to hold both.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		mux.HandleFunc(method+" /api/v1/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not found")
		})
	}
}

// writeError writes an error response in the shape every other failure uses, so that
// a rejection before the handler reads the same way to a client as one after it.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(ErrorResponse{Status: status, Message: message})
}

// labelsOf reads the labels off a request body, which carries none as a nil pointer.
func labelsOf(labels *api.Labels) map[string]string {
	if labels == nil {
		return nil
	}

	return *labels
}

// wireLabels reports the labels on a response, absent rather than an empty object
// when there are none, so that "unlabelled" and "not reported" are not the same value
// on the wire.
func wireLabels(labels map[string]string) *api.Labels {
	if len(labels) == 0 {
		return nil
	}

	wire := api.Labels(labels)

	return &wire
}

// internalError logs why a request failed and returns the message the client is told
// instead.
func internalError(logger *slog.Logger, operation string, err error) string {
	logger.With("error", err, "operation", operation).Error("failed to serve request")

	return "failed to " + operation
}

// authorize refuses a request whose caller does not meet the operation's
// declared requirement.
//
// The requirement is read from the request context, where the generated
// routing put the security scopes the OpenAPI document declares for the
// operation. The document is therefore the only copy of the requirements: an
// operation with no declaration is anonymous, one declaring no scope needs
// authentication but no role, and one naming a role needs a role that covers
// it. The bearer scheme's scopes are read, and the session scheme declares
// the same ones on every operation.
//
// The disabled identity passes everything, which is the network-boundary
// model unchanged. The recovery identity passes everything by design: it
// sits above policy, and a bad policy apply must not be able to lock the
// operator out.
func authorize(f api.StrictHandlerFunc, _ string) api.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		identity := middleware.CallerIdentity(ctx)
		if identity.Disabled || identity.Recovery {
			return f(ctx, w, r, request)
		}

		scopes, declared := ctx.Value(api.BearerScopes).([]string)
		if !declared {
			return f(ctx, w, r, request)
		}

		if identity.TokenID == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "authentication required")

			return nil, nil
		}

		for _, scope := range scopes {
			if !auth.Allows(identity.Role, manifest.Role(scope)) {
				writeError(w, http.StatusForbidden, "the "+scope+" role is required")

				return nil, nil
			}
		}

		return f(ctx, w, r, request)
	}
}

// keepOpen prepares a response that stays open for as long as something keeps
// writing to it, returning the writer to write through.
//
// Two things are needed for the same reason: the response is open for as long as
// the thing it reports on lives, which is longer than any deadline a request
// should have and longer than a caller can wait for a buffer to fill. So the
// write deadline is cleared, and every write is flushed.
//
// The deadline is set on the connection's own writer and the flushing is done
// through the handler's. Only the first can carry a deadline, and only the
// second counts what was written for the telemetry wrapped around it. The
// connection is nil when nothing put it there, and the deadline is then cleared
// on the handler's writer, which is the best that can be done.
//
// The zero time removes the deadline rather than extending it. A stream that
// hit one would end as a truncated response at exactly the timeout, which reads
// as a thing that stopped talking rather than as a server that hung up.
//
// Nothing is left unbounded by this. The request's context ends the stream when
// the caller disconnects, which is what actually limits how long it occupies
// the server.
//
// A writer with no deadline to clear says so, and there is nothing to do about
// that but carry on. The stream then lives as long as that writer allows, which
// is more than refusing to serve the request at all would give anybody.
func keepOpen(w, conn http.ResponseWriter) (io.Writer, error) {
	if conn == nil {
		conn = w
	}

	err := http.NewResponseController(conn).SetWriteDeadline(time.Time{})
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, fmt.Errorf("failed to clear the write deadline: %w", err)
	}

	return &flushWriter{inner: w, control: http.NewResponseController(w)}, nil
}

// The flushWriter type pushes each write out to the client rather than letting it sit
// in a buffer.
//
// Without this a stream arrives in chunks whenever the buffer happens to fill,
// which for a quiet source may be a long time after the line was written. Watching
// something happen is the whole point of a stream, so a line held back is a line
// that did not arrive.
type flushWriter struct {
	inner   io.Writer
	control *http.ResponseController
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if err != nil {
		return n, err
	}

	// A response that cannot be flushed is still a response. The output reaches the
	// caller when the buffer fills, which is worse than immediately and better than
	// failing the write over it.
	_ = w.control.Flush()

	return n, nil
}

// quoteETag wraps a tag in the quotes the ETag header carries.
func quoteETag(etag string) string {
	return `"` + etag + `"`
}

// unquoteETag strips the quotes an If-Match header carries, accepting a bare
// tag too so a caller pasting the value by hand is not refused over quoting.
func unquoteETag(etag string) string {
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag[1 : len(etag)-1]
	}

	return etag
}

// versionETag renders a resource's version as the tag a conditional apply
// hands back.
//
// The policy document derives its tag from its content, because it is one
// document with no row of its own to count writes against. A workload, volume
// or service already counts its writes, and that count moves only when an apply
// changed something, so it says the same thing more cheaply.
func versionETag(version int) string {
	return quoteETag(strconv.Itoa(version))
}

// parseIfMatch reads the version an If-Match header names, reporting whether
// the header was absent and whether what it carried was a version at all.
//
// An absent header means an unconditional apply, which is what creating a
// resource has to do: there is no tag yet to name. A header carrying something
// that is not one of takt's tags is a caller mistake rather than a conflict, so
// it is told apart from a version that simply no longer matches.
func parseIfMatch(header *string) (version int, conditional, ok bool) {
	if header == nil || *header == "" {
		return 0, false, true
	}

	version, err := strconv.Atoi(unquoteETag(*header))
	if err != nil || version <= 0 {
		return 0, true, false
	}

	return version, true, true
}
