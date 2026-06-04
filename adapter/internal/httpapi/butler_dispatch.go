package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/daboluocc/bbclaw/adapter/internal/butler"
)

// handleButlerDispatchRecent serves GET /v1/butler/dispatch/recent
//
// Returns the most recent butler dispatch tasks from the in-memory ring buffer
// (ADR-021-firmware-ui §1.4). Used by the firmware Task List page.
//
// Query params:
//   - limit: max entries to return (default 20, max 50)
//
// Response 200: bare JSON array of DispatchEntry, newest-first.
// Empty array when no dispatches recorded yet.
func (s *Server) handleButlerDispatchRecent(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			limit = n
		}
	}

	var entries []butler.DispatchEntry

	// Prefer DispatchRing (PR branch new path); fall back to DispatchRecorder (HEAD path).
	switch {
	case s.dispatchRing != nil:
		entries = s.dispatchRing.Recent(limit)
	case s.dispatchRecorder != nil:
		entries = s.dispatchRecorder.Recent(limit)
	}
	if entries == nil {
		entries = []butler.DispatchEntry{} // ensure JSON array, never null
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}
