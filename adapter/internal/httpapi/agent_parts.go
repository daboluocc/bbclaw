package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// handleAgentSessionParts returns a chronological page of a session's
// conversation history as STRUCTURED parts (thinking / text / tool / dispatch)
// for the admin conversation page (ADR-029 §2.1).
//
//	GET /v1/admin/sessions/{id}/parts?driver=claude-code&before=-1&limit=50
//	response: {"ok":true,"data":{
//	    "turns":[{"role":"assistant","seq":3,"parts":[
//	        {"kind":"thinking","text":"..."},
//	        {"kind":"text","text":"..."},
//	        {"kind":"dispatch","dispatch":{"taskId":"...","cwd":"...","status":"done"}}
//	    ]}],
//	    "total": 48, "hasMore": true
//	}}
//
// Pagination mirrors the messages endpoint: `before` is the upper-exclusive seq
// cursor over visible turns; a negative value means the latest page. Drivers
// that don't implement agent.PartLoader return PARTS_NOT_SUPPORTED so the page
// can fall back to the flat /messages endpoint.
func (s *Server) handleAgentSessionParts(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}

	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SESSION_ID_REQUIRED"})
		return
	}

	// ADR-014: resolve logical session ids to the underlying CLI session id.
	cliSessionID := sessionID
	if s.sessions != nil && strings.HasPrefix(sessionID, "ls-") {
		ls, ok := s.sessions.Get(logicalsession.ID(sessionID))
		if !ok {
			writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND", Detail: "logical session not found: " + sessionID})
			return
		}
		if ls.CLISessionID == "" {
			writeJSON(w, http.StatusOK, response{
				OK:   true,
				Data: map[string]any{"turns": []agent.Turn{}, "total": 0, "hasMore": false},
			})
			return
		}
		cliSessionID = ls.CLISessionID
	}

	driverName := strings.TrimSpace(r.URL.Query().Get("driver"))
	if driverName == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "DRIVER_REQUIRED"})
		return
	}

	drv, ok := s.router.Get(driverName)
	if !ok {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: "driver not registered: " + driverName})
		return
	}

	loader, ok := drv.(agent.PartLoader)
	if !ok {
		writeJSON(w, http.StatusOK, response{
			OK:     false,
			Error:  "PARTS_NOT_SUPPORTED",
			Detail: "driver " + driverName + " does not support structured part replay",
		})
		return
	}

	before := -1
	if v := strings.TrimSpace(r.URL.Query().Get("before")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_BEFORE"})
			return
		}
		before = n
	}

	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_LIMIT"})
			return
		}
		if n > maxMessagesPerPage {
			n = maxMessagesPerPage
		}
		limit = n
	}

	page, err := loader.LoadParts(r.Context(), cliSessionID, before, limit)
	if err != nil {
		s.log.Errorf("agent: load parts failed driver=%s sid=%s (logical=%s) err=%v", driverName, cliSessionID, sessionID, err)
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "LOAD_PARTS_FAILED", Detail: err.Error()})
		return
	}

	if page.Turns == nil {
		page.Turns = []agent.Turn{}
	}

	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"turns":   page.Turns,
			"total":   page.Total,
			"hasMore": page.HasMore,
		},
	})
}
