// Package adminui embeds the built Vue admin SPA and serves it under /admin.
//
// The SPA source lives in adapter/web (Vue 3 + Vite); `make web` (or
// `npm run build` there) writes the production bundle into ./dist, which is
// committed and embedded here so the adapter ships as a single self-contained
// binary — the Go build needs no Node toolchain. Rebuild dist whenever the
// frontend changes.
package adminui

import (
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"embed"
)

//go:embed all:dist
var distFS embed.FS

// content is the SPA file tree rooted at dist/.
var content, _ = fs.Sub(distFS, "dist")

// ServeHTTP serves the embedded SPA mounted at /admin. The Vite base is /admin/,
// so asset URLs resolve directly; any unknown sub-path falls back to index.html
// so the single-page app always loads. Caller gates this to localhost.
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin"), "/")
	if p == "" {
		p = "index.html"
	}
	data, err := fs.ReadFile(content, p)
	if err != nil {
		p = "index.html"
		data, err = fs.ReadFile(content, p)
		if err != nil {
			http.Error(w, "admin UI not built", http.StatusInternalServerError)
			return
		}
	}
	if ct := mime.TypeByExtension(filepath.Ext(p)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
