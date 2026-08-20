// Package webui serves the built-in console: one static page that drives the
// public REST API with a token the operator pastes into it.
//
// The assets are embedded in the binary, because installing processd has to
// stay "copy one file" (docs/SPEC.md §18).
package webui

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed assets
var assets embed.FS

// indexFile is served for the root of the mount point.
const indexFile = "index.html"

// Handler serves the console under the prefix it is mounted on.
//
// Only files that were embedded are served, and anything else is a 404: the
// console is a fixed set of assets, not a browsable directory.
func Handler() (http.Handler, error) {
	root, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, fmt.Errorf("opening console assets: %w", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = indexFile
		}

		// fs.ReadFile rejects any path that escapes the asset root, so a
		// traversal attempt ends as a plain 404.
		content, err := fs.ReadFile(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// The console ships with the daemon: a copy cached from an older
		// version would be talking to an API it does not know.
		w.Header().Set("Cache-Control", "no-cache")

		// ServeContent, not a file server: a file server answers /index.html
		// with a redirect, and the console is served, not browsed.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
	}), nil
}
