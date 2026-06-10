package httpapi

import (
	"net/http"
	"strconv"
)

// handleAdminLogs returns the most recent in-memory log lines plus the on-disk
// log file path, so the admin page's 日志 tab can show runtime output without
// the operator watching the binary's stdout (ADR-025). The lines come from the
// logger's bounded ring buffer; the file is where the same output is persisted
// (so a human or AI can tail it directly). Loopback-gated at the route layer.
//
//	GET /v1/admin/logs?limit=N   (default 600, max 2000)
//	→ {"ok":true,"data":{"file":"…/adapter-runtime.log","lines":[…]}}
func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	limit := 600
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 2000 {
		limit = 2000
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"file":  s.logFile,
		"lines": s.log.Recent(limit),
	}})
}
