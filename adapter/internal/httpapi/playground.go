package httpapi

import (
	_ "embed"
	"net/http"
)

// playgroundHTML is a single-file, zero-dependency web UI that hits
// /v1/agent/drivers + /v1/agent/message to let an operator dogfood
// whatever drivers are currently registered. Intentionally vanilla JS
// (no build step, no npm) so the adapter binary stays self-contained.
//
//go:embed playground.html
var playgroundHTML []byte

func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(playgroundHTML)
}
