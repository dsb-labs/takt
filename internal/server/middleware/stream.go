package middleware

import (
	"context"
	"net/http"
)

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
