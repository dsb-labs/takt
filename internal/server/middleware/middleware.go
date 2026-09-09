// Package middleware provides the HTTP middleware the server puts in front of
// its API and web UI. Each middleware lives in its own file, and Wrap holds
// the order they compose in.
package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

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
func Wrap(handler http.Handler, logger *slog.Logger, hosts []string, authenticator Authenticator) http.Handler {
	for _, middleware := range []func(http.Handler) http.Handler{
		Recovery(logger),
		// Outside Recovery, so the 500 a recovered panic writes goes through
		// the compressor the response's headers already promised.
		Gzip,
		Logging(logger),
		// Directly inside Guard, so a credential is only ever read from a request
		// that named this server. A nil authenticator means the configuration
		// carries no [auth] block, and every request proceeds as it did before
		// the layer existed.
		Authenticate(authenticator),
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

// writeError writes an error response in the shape the API returns for every
// failure, so a rejection before the handler reads the same way to a client as
// one after it. The struct mirrors the API's ErrorResponse rather than
// importing it, because the API is a consumer of this package.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"error"`
	}{Message: message})
}
