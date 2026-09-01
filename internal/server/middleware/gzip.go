package api

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// Gzip returns middleware that compresses a response when the client says it
// accepts that.
//
// The body is compressed as it is written rather than buffered, so a streamed
// response — a followed log read — stays a stream: a flush from the handler
// flushes the compressor before the connection. A request naming a byte range
// is served uncompressed, because a range into a file and a range into its
// compressed form are different bytes.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The header value can carry several codings with parameters, so this
		// looks for the name rather than comparing the whole value.
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)

			return
		}

		// Whether the body is compressed depends on this request's headers, so
		// a cache holding the answer has to key on them.
		w.Header().Add("Vary", "Accept-Encoding")

		gz := &gzipResponseWriter{ResponseWriter: w}
		defer gz.Close()

		next.ServeHTTP(gz, r)
	})
}

// The gzipResponseWriter type compresses what a handler writes. The decision to
// compress is made when the header is written, because that is the last moment
// the encoding can still be declared.
type gzipResponseWriter struct {
	http.ResponseWriter
	writer      *gzip.Writer
	compressing bool
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		w.ResponseWriter.WriteHeader(status)

		return
	}

	w.wroteHeader = true

	// A response with no body gets no encoding: an empty gzip stream is still
	// bytes, and these statuses forbid any. One already carrying an encoding is
	// left as the handler declared it.
	if status == http.StatusNoContent || status == http.StatusNotModified ||
		w.Header().Get("Content-Encoding") != "" {
		w.ResponseWriter.WriteHeader(status)

		return
	}

	// The length the handler knew is the uncompressed one, which is no longer
	// the length of the body.
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	w.compressing = true

	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if !w.compressing {
		return w.ResponseWriter.Write(p)
	}

	if w.writer == nil {
		w.writer = gzip.NewWriter(w.ResponseWriter)
	}

	return w.writer.Write(p)
}

// Flush sends what the compressor holds, then flushes the connection. Without
// the first step a followed log read would sit in the compressor's buffer for
// as long as the workload stays quiet.
func (w *gzipResponseWriter) Flush() {
	if w.writer != nil {
		_ = w.writer.Flush()
	}

	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Close ends the compressed stream. Without it the final block never reaches
// the client and the body does not decode.
func (w *gzipResponseWriter) Close() {
	if w.writer != nil {
		_ = w.writer.Close()
	}
}

// Unwrap returns the writer underneath, which is how http.ResponseController
// reaches what this wrapper does not implement itself.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
