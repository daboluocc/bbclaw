package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/daboluocc/bbclaw/adapter/internal/butler"
)

// SetDispatchRecorder wires the process-level dispatch ring buffer into the
// server so GET /v1/butler/dispatch/recent has data to serve.
func (s *Server) SetDispatchRecorder(r *butler.DispatchRecorder) {
	s.dispatchRecorder = r
}

// handleButlerDispatchRecent serves GET /v1/butler/dispatch/recent.
// Returns up to 20 recent dispatch entries (newest first).
// Query param: ?limit=N to override the default cap (max 50).
//
// Response: JSON array of DispatchEntry. Empty ring buffer → [].
func (s *Server) handleButlerDispatchRecent(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			limit = n
		}
	}

	var entries []butler.DispatchEntry
	if s.dispatchRecorder != nil {
		entries = s.dispatchRecorder.Recent(limit)
	} else {
		entries = []butler.DispatchEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}
