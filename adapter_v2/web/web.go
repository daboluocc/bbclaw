// Package web embeds and serves the minimal Phase 1 browser client (issue
// #208): a single static index.html that runs xterm.js + the fit addon from a
// CDN and drives an interactive CLI over the adapter's /ws endpoint.
//
// The page is committed and embedded here so the adapter ships as one
// self-contained binary with NO Node/Vite toolchain at build time — `go build`
// alone produces a runnable server that serves the client. Edit index.html in
// place and rebuild; there is no compile step for the asset.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// staticFS holds the embedded static client tree (index.html + admin.html).
// The embed pattern is relative to this file, so the assets live alongside it
// under adapter_v2/web/.
//
//go:embed index.html admin.html
var staticFS embed.FS

// Handler returns an http.Handler that serves the embedded web client. Mount it
// at the site root: a request for "/" yields index.html (http.FileServer's
// directory-index behaviour), and the page itself opens the WebSocket to
// "/ws?session=<id>", so no other static routes are needed.
//
// It is a plain http.FileServer over the embedded FS — the client is fully
// static, so there is nothing to template or generate at request time.
func Handler() http.Handler {
	// staticFS is rooted at the package dir; serve its contents directly. fs.Sub
	// with "." is a no-op today but keeps the served root explicit if more files
	// (icons, a manifest) are added under web/ later.
	sub, err := fs.Sub(staticFS, ".")
	if err != nil {
		// embed.FS with a literal "." sub can't fail; treat any future breakage
		// as a programming error rather than a runtime condition to handle.
		panic("web: sub embedded FS: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// AdminHandler returns an http.Handler that serves the embedded admin/config
// page (admin.html) for /admin and /admin/. Unlike the static file server it
// serves the single page regardless of the trailing slash and sends
// Cache-Control: no-store so a settings change is never masked by a cached page.
// The admin route is loopback-gated by the caller (see adminapi.LocalOnly).
func AdminHandler() http.Handler {
	page, err := staticFS.ReadFile("admin.html")
	if err != nil {
		panic("web: read embedded admin.html: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	})
}
