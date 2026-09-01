package middleware

import "net/http"

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
