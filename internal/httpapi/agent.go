package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// SetAgentDriver attaches an agent driver to the server. Pass nil to disable
// the /v1/agent/* endpoints. Phase 1 accepts a single driver; a future phase
// will swap this for a Router that multiplexes named drivers.
func (s *Server) SetAgentDriver(d agent.Driver) {
	s.agent = d
}

type agentMessageRequest struct {
	Text string `json:"text"`
}

// handleAgentMessage streams one agent turn as NDJSON.
//
//	POST /v1/agent/message
//	{"text":"hello"}
//
//	response: application/x-ndjson
//	  {"type":"text","text":"..."}
//	  {"type":"tokens","in":N,"out":M}
//	  {"type":"turn_end"}
//
// Phase 1: each call is an independent one-shot turn (new claude-code
// subprocess, no session reuse across requests).
func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}

	var req agentMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_TEXT"})
		return
	}

	sw, ok := newFinishStreamWriter(w)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "STREAMING_NOT_SUPPORTED"})
		return
	}

	ctx := r.Context()
	sid, err := s.agent.Start(ctx, agent.StartOpts{})
	if err != nil {
		_ = sw.write(map[string]any{"type": "error", "error": "AGENT_START_FAILED", "detail": err.Error()})
		return
	}
	defer func() { _ = s.agent.Stop(sid) }()

	events := s.agent.Events(sid)

	// Send blocks until the subprocess exits and emits EvTurnEnd. Run it in
	// a goroutine so we can drain events concurrently.
	sendErrCh := make(chan error, 1)
	go func() { sendErrCh <- s.agent.Send(sid, text) }()

	s.metrics.Inc("agent_message_start")
	s.log.Infof("phase=agent_start sid=%s text_chars=%d", sid, len(text))

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !s.writeAgentEvent(sw, ev) {
				return
			}
			if ev.Type == agent.EvTurnEnd {
				// Wait for Send to return so we log its error properly.
				if sendErr := <-sendErrCh; sendErr != nil {
					s.log.Errorf("phase=agent_send_failed sid=%s err=%v", sid, sendErr)
				}
				s.metrics.Inc("agent_message_ok")
				s.log.Infof("phase=agent_done sid=%s", sid)
				return
			}
		}
	}
}

// writeAgentEvent serialises an agent.Event to the NDJSON stream.
// Returns false if the write fails (client disconnected).
func (s *Server) writeAgentEvent(sw *finishStreamWriter, ev agent.Event) bool {
	frame := map[string]any{"type": string(ev.Type), "seq": ev.Seq}
	switch ev.Type {
	case agent.EvText, agent.EvError, agent.EvStatus:
		frame["text"] = ev.Text
	case agent.EvTokens:
		if ev.Tokens != nil {
			frame["in"] = ev.Tokens.In
			frame["out"] = ev.Tokens.Out
		}
	case agent.EvToolCall:
		if ev.Tool != nil {
			frame["id"] = string(ev.Tool.ID)
			frame["tool"] = ev.Tool.Tool
			frame["hint"] = ev.Tool.Hint
		}
	case agent.EvTurnEnd:
		// no extra fields
	}
	if err := sw.write(frame); err != nil {
		s.log.Warnf("agent: write frame failed: %v", err)
		return false
	}
	return true
}
