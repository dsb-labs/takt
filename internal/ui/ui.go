// Package ui serves the web UI embedded in the takt binary.
//
// The UI is a single-page application built from the sources under app/ into
// dist/, which is embedded at build time. A binary built without the bundle
// still compiles, and answers every page request with a message saying how to
// get one.
package ui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler returns the handler serving the embedded web UI.
func Handler() (http.Handler, error) {
	files, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("failed to open the embedded bundle: %w", err)
	}

	return NewHandler(files), nil
}

// NewHandler returns a handler serving the web UI from the given filesystem.
//
// A path naming a file in the bundle is served as-is. Every other path is
// answered with the application's index page: the application routes on the
// client, so a deep link has to load the page before the router can read the
// path. The bundler puts a content hash in every asset's name, so assets are
// cached indefinitely where the index page is revalidated on every load —
// which is what makes a new binary's UI arrive without a hard refresh.
func NewHandler(files fs.FS) http.Handler {
	static := http.FileServerFS(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		// The index page is excluded so that a request naming it directly still
		// gets the fallback's caching headers.
		if info, err := fs.Stat(files, path); path != "" && path != "index.html" && err == nil && !info.IsDir() {
			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}

			static.ServeHTTP(w, r)

			return
		}

		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "this build does not include the web ui", http.StatusNotFound)

			return
		}

		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		_, _ = w.Write(index)
	})
}
