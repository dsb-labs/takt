// Package api provides the HTTP API surface of the takt server.
//
// The routes, request decoding and response marshalling are generated from
// api/openapi.yaml. This package implements the generated interface. The
// middleware the server wraps around it lives in the middleware package.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/dsb-labs/takt/internal/generated/api"
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
	api.HandlerWithOptions(api.NewStrictHandler(a, nil), api.StdHTTPServerOptions{
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
