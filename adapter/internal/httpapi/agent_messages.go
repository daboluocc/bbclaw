package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// Per-call cap on the limit query parameter. Devices have small displays and
// limited memory; refuse to ship more than this in one HTTP response.
const maxMessagesPerPage = 200

// handleAgentSessionMessages returns a chronological page of a session's
// conversation history.
//
//	GET /v1/agent/sessions/{id}/messages?driver=claudecode&before=-1&limit=50
//	response: {"ok":true,"data":{
//	    "messages":[{"role":"user","content":"...","seq":120},...],
//	    "total": 348,
//	    "hasMore": true
//	}}
//
// Pagination cursor: `before` is the upper-exclusive seq cursor. A negative
// value (the typical first call) means "the latest page" — return the last
// `limit` messages. To page backward, pass before=<smallest seq returned so
// far>. hasMore is true iff earlier messages remain.
//
// Drivers that don't implement agent.MessageLoader return MESSAGES_NOT_SUPPORTED
// so the device can degrade gracefully (empty transcript, same as today).
func (s *Server) handleAgentSessionMessages(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}

	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SESSION_ID_REQUIRED"})
		return
	}

	// ADR-014: resolve logical session ids to the underlying CLI session id
	// so the driver's MessageLoader receives the id it actually knows about
	// (the CLI conversation UUID, not the stable ls- id).
	cliSessionID := sessionID
	if s.sessions != nil && strings.HasPrefix(sessionID, "ls-") {
		ls, ok := s.sessions.Get(logicalsession.ID(sessionID))
		if !ok {
			writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND", Detail: "logical session not found: " + sessionID})
			return
		}
		if ls.CLISessionID == "" {
			// Logical session exists but no CLI conversation has been started
			// yet (session was minted via POST but no message sent). Return an
			// empty page rather than an error — the device can handle this.
			writeJSON(w, http.StatusOK, response{
				OK: true,
				Data: map[string]any{
					"messages": []agent.Message{},
					"total":    0,
					"hasMore":  false,
				},
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
		writeJSON(w, http.StatusBadRequest, response{
			OK:     false,
			Error:  "UNKNOWN_DRIVER",
			Detail: "driver not registered: " + driverName,
		})
		return
	}

	loader, ok := drv.(agent.MessageLoader)
	if !ok {
		writeJSON(w, http.StatusOK, response{
			OK:    false,
			Error: "MESSAGES_NOT_SUPPORTED",
			Detail: "driver " + driverName + " does not support message replay",
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

	page, err := loader.LoadMessages(r.Context(), cliSessionID, before, limit)
	if err != nil {
		s.log.Errorf("agent: load messages failed driver=%s sid=%s (logical=%s) err=%v", driverName, cliSessionID, sessionID, err)
		writeJSON(w, http.StatusInternalServerError, response{
			OK:     false,
			Error:  "LOAD_MESSAGES_FAILED",
			Detail: err.Error(),
		})
		return
	}

	// Always return a non-nil slice so the client doesn't have to special-case null.
	if page.Messages == nil {
		page.Messages = []agent.Message{}
	}

	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"messages": page.Messages,
			"total":    page.Total,
			"hasMore":  page.HasMore,
		},
	})
}
