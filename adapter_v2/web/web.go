// Package web embeds and serves the adapter_v2 browser client: a Vue 3 + Vite
// single-page app (Terminal + Sessions views) that drives the interactive CLI
// over the adapter's /ws endpoint and reads the agent-session HTTP API.
//
// The SPA source lives in web/spa (Vue 3 + Vite + TypeScript). `make web` (or
// `npm run build` there) writes the production bundle into web/dist, which is
// committed and embedded here so the adapter ships as one self-contained binary
// with NO Node/Vite toolchain at build time — `go build` alone produces a
// runnable server. Rebuild web/dist whenever the frontend changes.
//
// NOTE: `//go:embed all:dist` requires web/dist to exist and be non-empty at
// build time. A clean checkout must run `make web` before `go build`.
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// distFS holds the embedded built SPA tree (web/dist) plus the admin page.
// The embed pattern is relative to this file, so the assets live alongside it
// under adapter_v2/web/. `all:dist` includes files Vite hashes into dist/assets
// (and any dotfiles), unlike a bare `dist` pattern.
//
//go:embed all:dist admin.html
var distFS embed.FS

// content is the SPA file tree rooted at dist/.
var content, _ = fs.Sub(distFS, "dist")

// Handler returns an http.Handler that serves the embedded SPA mounted at the
// site root ("/"). The Vite base is "/", so hashed asset URLs resolve directly;
// any path that is not an existing dist asset falls back to index.html so the
// client-side routes (e.g. "#sessions") always load the single-page app. The
// page itself opens the WebSocket to "/ws?session=<id>".
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		data, err := fs.ReadFile(content, p)
		if err != nil {
			// SPA fallback: any unknown path serves index.html so deep links and
			// client routes resolve to the app shell.
			p = "index.html"
			data, err = fs.ReadFile(content, p)
			if err != nil {
				http.Error(w, "web UI not built (run `make web`)", http.StatusInternalServerError)
				return
			}
		}
		if ct := mime.TypeByExtension(filepath.Ext(p)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		// Hashed assets are immutable; the shell must not be cached so a new
		// build is picked up. Keep it simple and no-store the shell.
		if p == "index.html" {
			w.Header().Set("Cache-Control", "no-store")
		}
		_, _ = w.Write(data)
	})
}

// AdminHandler returns an http.Handler that serves the embedded admin/config
// page (admin.html) for /admin and /admin/. It serves the single page
// regardless of the trailing slash and sends Cache-Control: no-store so a
// settings change is never masked by a cached page. The admin route is
// loopback-gated by the caller (see adminapi.LocalOnly).
func AdminHandler() http.Handler {
	page, err := distFS.ReadFile("admin.html")
	if err != nil {
		panic("web: read embedded admin.html: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	})
}
