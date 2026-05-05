package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// approveRequest is the body for POST /v1/agent/sessions/{id}/approve.
type approveRequest struct {
	ToolID   string `json:"toolId"`
	Decision string `json:"decision"`
}

// handleAgentSessionApprove forwards a tool-approval decision to the driver
// that owns the session.
//
//	POST /v1/agent/sessions/{id}/approve
//	{"toolId":"<from tool_call event>","decision":"once"|"deny"}
//	→ {"ok":true}
//
// Error codes:
//   - SESSION_ID_REQUIRED  — path param missing
//   - SESSION_NOT_FOUND    — logical or CLI session unknown
//   - INVALID_REQUEST      — bad JSON or missing fields
//   - TOOL_APPROVAL_NOT_SUPPORTED — driver's Capabilities.ToolApproval is false
//   - AGENT_NOT_CONFIGURED — router not set
func (s *Server) handleAgentSessionApprove(w http.ResponseWriter, r *http.Request) {
	if s.router == nil || s.agentSessions == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}

	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SESSION_ID_REQUIRED"})
		return
	}

	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	req.ToolID = strings.TrimSpace(req.ToolID)
	req.Decision = strings.TrimSpace(req.Decision)
	if req.ToolID == "" || req.Decision == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: "toolId and decision are required"})
		return
	}

	// Resolve logical session id → CLI session id + driver name.
	cliSessionID := sessionID
	driverName := ""

	if s.sessions != nil && strings.HasPrefix(sessionID, "ls-") {
		ls, ok := s.sessions.Get(logicalsession.ID(sessionID))
		if !ok {
			writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
			return
		}
		if ls.CLISessionID == "" {
			// Logical session exists but no CLI turn has happened yet — nothing
			// to approve.
			writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND", Detail: "no active CLI session for this logical session"})
			return
		}
		cliSessionID = ls.CLISessionID
		driverName = ls.Driver
	}

	// Look up the runtime entry to confirm the session is live and to get the
	// driver name when we didn't get it from the logical table.
	entry, found := s.agentSessions.get(cliSessionID)
	if !found {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
		return
	}
	if driverName == "" {
		driverName = entry.driverName
	}

	drv, ok := s.router.Get(driverName)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: driverName})
		return
	}

	// Reject early if the driver doesn't advertise tool approval support.
	if !drv.Capabilities().ToolApproval {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "TOOL_APPROVAL_NOT_SUPPORTED"})
		return
	}

	decision := agent.Decision(req.Decision)
	if decision != agent.DecisionOnce && decision != agent.DecisionDeny {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: "decision must be 'once' or 'deny'"})
		return
	}

	if err := drv.Approve(entry.sid, agent.ToolID(req.ToolID), decision); err != nil {
		if errors.Is(err, agent.ErrUnsupported) {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "TOOL_APPROVAL_NOT_SUPPORTED"})
			return
		}
		if errors.Is(err, agent.ErrUnknownSession) {
			writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "APPROVE_FAILED", Detail: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, response{OK: true})
}

// handleAgentSessionGet returns metadata for a single logical session.
//
//	GET /v1/agent/sessions/{id}
//	→ {"ok":true,"data":{"session":{...,"state":"idle"|"running"|"completed"|"error"}}}
//
// The `state` field is sourced from the runtime registry (agentSessions). If
// the adapter has restarted and the session is no longer in the registry,
// state defaults to "idle".
//
// Error codes:
//   - SESSION_ID_REQUIRED       — path param missing
//   - NOT_LOGICAL               — id does not have ls- prefix
//   - SESSION_NOT_FOUND         — logical session unknown
//   - LOGICAL_SESSIONS_DISABLED — session manager not configured
//   - AGENT_NOT_CONFIGURED      — router not set
func (s *Server) handleAgentSessionGet(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	if s.sessions == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
		return
	}

	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SESSION_ID_REQUIRED"})
		return
	}
	if !strings.HasPrefix(sessionID, "ls-") {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "NOT_LOGICAL", Detail: "id must have ls- prefix"})
		return
	}

	ls, ok := s.sessions.Get(logicalsession.ID(sessionID))
	if !ok {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
		return
	}

	// Derive runtime state from the registry. Fall back to "idle" when the
	// adapter has restarted and the CLI session is no longer tracked.
	state := "idle"
	if s.agentSessions != nil && ls.CLISessionID != "" {
		if entry, found := s.agentSessions.get(ls.CLISessionID); found {
			state = entry.state
		}
	}

	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"session": map[string]any{
				"id":           string(ls.ID),
				"deviceId":     ls.DeviceID,
				"driver":       ls.Driver,
				"cwd":          ls.Cwd,
				"title":        ls.Title,
				"createdAt":    ls.CreatedAt,
				"lastUsedAt":   ls.LastUsedAt,
				"cliSessionId": ls.CLISessionID,
				"state":        state,
			},
		},
	})
}
