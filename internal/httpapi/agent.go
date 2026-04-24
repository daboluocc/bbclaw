package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// Session sweeper tunables. Sessions inactive for more than sessionTTL are
// evicted by the sweeper goroutine which runs every sweepInterval.
const (
	sessionTTL    = 30 * time.Minute
	sweepInterval = 5 * time.Minute
)

// sessionEntry tracks one live agent session held open across HTTP turns.
type sessionEntry struct {
	sid      agent.SessionID
	lastUsed time.Time
}

// sessionRegistry is a goroutine-safe map of server-visible session id
// (the string form of agent.SessionID) to its entry.
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]*sessionEntry)}
}

func (r *sessionRegistry) get(id string) (*sessionEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[id]
	return e, ok
}

func (r *sessionRegistry) put(id string, e *sessionEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = e
}

// touch updates lastUsed for id if present. Safe to call on missing ids.
func (r *sessionRegistry) touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[id]; ok {
		e.lastUsed = time.Now()
	}
}

// snapshotExpired returns and removes all entries whose lastUsed is older
// than now-ttl. The caller is responsible for calling Driver.Stop on each.
func (r *sessionRegistry) snapshotExpired(ttl time.Duration) []*sessionEntry {
	cutoff := time.Now().Add(-ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*sessionEntry
	for id, e := range r.sessions {
		if e.lastUsed.Before(cutoff) {
			out = append(out, e)
			delete(r.sessions, id)
		}
	}
	return out
}

// SetAgentDriver attaches an agent driver to the server. Pass nil to disable
// the /v1/agent/* endpoints. Phase 1.5 adds multi-turn session continuity:
// sessions persist across HTTP requests and are evicted by a background
// sweeper after sessionTTL of inactivity.
func (s *Server) SetAgentDriver(d agent.Driver) {
	s.agent = d
	if d == nil {
		if s.agentCancel != nil {
			s.agentCancel()
			s.agentCancel = nil
		}
		s.agentCtx = nil
		s.agentSessions = nil
		return
	}
	// NOTE: the Server struct has no Shutdown hook in Phase 1.5, so in
	// practice agentCtx lives for the process lifetime. That is acceptable
	// here because the sweeper goroutine is the only long-lived consumer
	// and the process exits with the parent. When a shutdown hook is added
	// later, call s.agentCancel() from there to tear everything down.
	s.agentCtx, s.agentCancel = context.WithCancel(context.Background())
	s.agentSessions = newSessionRegistry()
	go s.runSessionSweeper(s.agentCtx, sweepInterval, sessionTTL)
}

// runSessionSweeper periodically evicts sessions that have been idle for
// longer than ttl. Exits when ctx is done.
func (s *Server) runSessionSweeper(ctx context.Context, interval, ttl time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepSessions(ttl)
		}
	}
}

// sweepSessions removes idle sessions from the registry and stops them on
// the driver. Safe to call concurrently with request handling.
func (s *Server) sweepSessions(ttl time.Duration) {
	if s.agentSessions == nil || s.agent == nil {
		return
	}
	for _, e := range s.agentSessions.snapshotExpired(ttl) {
		if err := s.agent.Stop(e.sid); err != nil {
			s.log.Warnf("agent: sweep stop sid=%s err=%v", e.sid, err)
			continue
		}
		s.log.Infof("agent: swept idle sid=%s", e.sid)
	}
}

type agentMessageRequest struct {
	Text      string `json:"text"`
	SessionId string `json:"sessionId,omitempty"`
}

// handleAgentMessage streams one agent turn as NDJSON.
//
//	POST /v1/agent/message
//	{"text":"hello","sessionId":"<optional>"}
//
//	response: application/x-ndjson
//	  {"type":"session","sessionId":"...","isNew":true|false,"seq":0}
//	  {"type":"text","text":"..."}
//	  {"type":"tokens","in":N,"out":M}
//	  {"type":"turn_end"}
//
// Phase 1.5: if sessionId is provided and matches a live entry, the
// existing driver session is reused (multi-turn continuity). Otherwise a
// fresh driver session is started. Sessions are NOT stopped on turn_end;
// a background sweeper evicts them after sessionTTL of inactivity.
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

	// Resolve or create the backing session. We key the registry by the
	// string form of agent.SessionID so the device-visible id and the
	// driver-internal id are the same value.
	var (
		sid   agent.SessionID
		isNew bool
	)
	if requested := strings.TrimSpace(req.SessionId); requested != "" {
		if entry, found := s.agentSessions.get(requested); found {
			sid = entry.sid
			isNew = false
		}
	}
	if sid == "" {
		newSid, err := s.agent.Start(s.agentCtx, agent.StartOpts{})
		if err != nil {
			_ = sw.write(map[string]any{"type": "error", "error": "AGENT_START_FAILED", "detail": err.Error()})
			return
		}
		sid = newSid
		isNew = true
		s.agentSessions.put(string(sid), &sessionEntry{sid: sid, lastUsed: time.Now()})
	}

	// Emit the session frame first so the client learns (or confirms) the
	// sessionId before any text arrives.
	if err := sw.write(map[string]any{
		"type":      "session",
		"sessionId": string(sid),
		"isNew":     isNew,
		"seq":       0,
	}); err != nil {
		s.log.Warnf("agent: write session frame failed: %v", err)
		return
	}

	// Bump lastUsed now that we've committed to serving this turn.
	s.agentSessions.touch(string(sid))

	events := s.agent.Events(sid)

	// Send blocks until the subprocess exits and emits EvTurnEnd. Run it in
	// a goroutine so we can drain events concurrently.
	sendErrCh := make(chan error, 1)
	go func() { sendErrCh <- s.agent.Send(sid, text) }()

	s.metrics.Inc("agent_message_start")
	s.log.Infof("phase=agent_start sid=%s is_new=%v text_chars=%d", sid, isNew, len(text))

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Driver closed the channel (session ended on its side);
				// drop the registry entry so we don't resurrect a dead sid.
				s.agentSessions.mu.Lock()
				delete(s.agentSessions.sessions, string(sid))
				s.agentSessions.mu.Unlock()
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
				// Turn ended cleanly: keep the session alive for the next
				// request and refresh lastUsed.
				s.agentSessions.touch(string(sid))
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
