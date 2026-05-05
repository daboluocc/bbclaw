package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// Session sweeper tunables. Sessions inactive for more than sessionTTL are
// evicted by the sweeper goroutine which runs every sweepInterval.
const (
	sessionTTL    = 30 * time.Minute
	sweepInterval = 5 * time.Minute
)

// sessionEntry tracks one live agent session held open across HTTP turns.
// driverName records which router entry owns the underlying driver session
// so sweeps + subsequent turns always route to the right Driver.
type sessionEntry struct {
	sid        agent.SessionID
	driverName string
	lastUsed   time.Time
	state      string // "idle", "running", "completed", "error"
	lastEvent  string // last event type for status queries
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

func (r *sessionRegistry) setState(id string, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[id]; ok {
		e.state = state
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

// SetAgentDriver is the Phase 1.5 convenience wrapper: it builds a
// single-driver Router internally and forwards to SetAgentRouter. Kept for
// backward compatibility with existing tests and the main binary's previous
// wiring.
func (s *Server) SetAgentDriver(d agent.Driver) {
	if d == nil {
		s.SetAgentRouter(nil)
		return
	}
	r := agent.NewRouter()
	r.Register(d, s.log)
	s.SetAgentRouter(r)
}

// SetSessionManager attaches the logical-session table (ADR-014). When set,
// inbound sessionId fields prefixed "ls-" are resolved through the manager
// to the underlying CLI session id, and SESSION_NOT_FOUND retries write the
// new CLI id back. nil disables the manager-aware path entirely.
func (s *Server) SetSessionManager(m *logicalsession.Manager) { s.sessions = m }

// SetAgentRouter attaches a multi-driver router to the server. Pass nil to
// disable the /v1/agent/* endpoints. Starts the long-lived session sweeper
// the same way Phase 1.5 did.
func (s *Server) SetAgentRouter(r *agent.Router) {
	s.router = r
	if r == nil || r.Default() == nil {
		if s.agentCancel != nil {
			s.agentCancel()
			s.agentCancel = nil
		}
		s.agentCtx = nil
		s.agentSessions = nil
		s.router = nil
		return
	}
	s.agentCtx, s.agentCancel = context.WithCancel(context.Background())
	s.agentSessions = newSessionRegistry()
	s.wsHub = newWSHub(s.log)
	s.notifQueue = newNotificationQueue(32)
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
// the driver pinned to each entry. Safe to call concurrently with request
// handling.
func (s *Server) sweepSessions(ttl time.Duration) {
	if s.agentSessions == nil || s.router == nil {
		return
	}
	for _, e := range s.agentSessions.snapshotExpired(ttl) {
		drv, ok := s.router.Get(e.driverName)
		if !ok {
			s.log.Warnf("agent: sweep found entry with unknown driver=%s sid=%s", e.driverName, e.sid)
			continue
		}
		if err := drv.Stop(e.sid); err != nil {
			s.log.Warnf("agent: sweep stop driver=%s sid=%s err=%v", e.driverName, e.sid, err)
			continue
		}
		s.log.Infof("agent: swept idle driver=%s sid=%s", e.driverName, e.sid)
	}
}

// Shutdown stops the session sweeper and cleanly terminates every live
// agent session. Safe to call more than once — subsequent calls are no-ops.
// ctx is honoured for the per-driver Stop calls; if it expires the
// remaining sessions are abandoned rather than blocked on.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.agentCancel == nil {
		return nil
	}
	s.agentCancel()
	s.agentCancel = nil

	if s.agentSessions == nil || s.router == nil {
		return nil
	}
	// Drain the registry atomically so handlers in flight don't resurrect
	// sessions we're trying to stop.
	s.agentSessions.mu.Lock()
	entries := s.agentSessions.sessions
	s.agentSessions.sessions = make(map[string]*sessionEntry)
	s.agentSessions.mu.Unlock()

	if len(entries) == 0 {
		return nil
	}
	s.log.Infof("agent: shutdown stopping %d live session(s)", len(entries))
	for id, e := range entries {
		select {
		case <-ctx.Done():
			s.log.Warnf("agent: shutdown deadline reached, %d sessions not cleanly stopped", len(entries))
			return ctx.Err()
		default:
		}
		d, ok := s.router.Get(e.driverName)
		if !ok {
			continue
		}
		if err := d.Stop(e.sid); err != nil {
			s.log.Warnf("agent: shutdown stop sid=%s driver=%s err=%v", id, e.driverName, err)
		}
	}
	return nil
}

type agentMessageRequest struct {
	Text      string `json:"text"`
	SessionId string `json:"sessionId,omitempty"`
	Driver    string `json:"driver,omitempty"`
}

// handleAgentSessions lists persisted sessions for a given driver.
//
//	GET /v1/agent/sessions?driver=claude-code&limit=6
//	response: {"ok":true,"data":{"sessions":[{"id":"...","preview":"...","lastUsed":1714000000,"messageCount":8}]}}
//
// The optional `kind=logical` query switches the source from the driver's
// own session list (CLI-native) to the logical-session manager (ADR-014).
// When kind=logical, deviceId/driver are filters rather than required, and
// the response carries the LogicalSession shape instead of SessionInfo.
func (s *Server) handleAgentSessions(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}

	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind == "logical" {
		s.handleAgentSessionsLogical(w, r)
		return
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

	// Check if driver implements SessionLister
	lister, ok := drv.(agent.SessionLister)
	if !ok {
		// Driver doesn't implement SessionLister — return empty list, not an error
		writeJSON(w, http.StatusOK, response{
			OK:   true,
			Data: map[string]any{"sessions": []agent.SessionInfo{}},
		})
		return
	}

	limit := 6 // default
	if limitStr := strings.TrimSpace(r.URL.Query().Get("limit")); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	sessions, err := lister.ListSessions(r.Context(), limit)
	if err != nil {
		s.log.Errorf("agent: list sessions failed driver=%s err=%v", driverName, err)
		writeJSON(w, http.StatusInternalServerError, response{
			OK:     false,
			Error:  "LIST_SESSIONS_FAILED",
			Detail: err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: map[string]any{"sessions": sessions},
	})
}

// handleAgentDrivers lists the drivers currently registered on the router.
//
//	GET /v1/agent/drivers
//	response: {"ok":true,"data":{"drivers":[{"name":"...","capabilities":{...}},...]}}
func (s *Server) handleAgentDrivers(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	drivers := s.router.List()
	// Stable order makes the response testable and easier to eyeball.
	sort.Slice(drivers, func(i, j int) bool { return drivers[i].Name < drivers[j].Name })
	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: map[string]any{"drivers": drivers},
	})
}

// handleAgentMessage streams one agent turn as NDJSON.
//
//	POST /v1/agent/message
//	{"text":"hello","sessionId":"<optional>","driver":"<optional>"}
//
//	response: application/x-ndjson
//	  {"type":"session","sessionId":"...","isNew":true|false,"seq":0}
//	  {"type":"text","text":"..."}
//	  {"type":"tokens","in":N,"out":M}
//	  {"type":"turn_end"}
//
// Phase 3: routes to one of the registered drivers. When the request
// includes a sessionId for an existing entry, the driver is pinned to that
// session (see SESSION_DRIVER_MISMATCH handling below).
func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
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
	requestedDriver := strings.TrimSpace(req.Driver)

	// Resolve an initial candidate driver. If the caller named one, it must
	// exist; otherwise we fall back to the router default. A reused session
	// may still override this below because sessions are pinned to whichever
	// driver started them.
	var (
		drv        agent.Driver
		driverName string
	)
	if requestedDriver != "" {
		var ok bool
		drv, ok = s.router.Get(requestedDriver)
		if !ok {
			writeJSON(w, http.StatusBadRequest, response{
				OK:     false,
				Error:  "UNKNOWN_DRIVER",
				Detail: "driver not registered: " + requestedDriver,
			})
			return
		}
		driverName = requestedDriver
	} else {
		drv = s.router.Default()
		if drv == nil {
			writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
			return
		}
		driverName = drv.Name()
	}

	// Resolve or create the backing session. We key the registry by the
	// string form of agent.SessionID so the device-visible id and the
	// driver-internal id are the same value.
	//
	// Session lookup + pinning validation happens BEFORE we flip the
	// response to NDJSON so we can still emit a plain JSON 4xx for
	// SESSION_DRIVER_MISMATCH / UNKNOWN_DRIVER.
	var (
		sid   agent.SessionID
		isNew bool
	)
	requestedSession := strings.TrimSpace(req.SessionId)

	// ADR-014 Phase B: when a logical-session manager is configured, resolve
	// "ls-"-prefixed ids and auto-mint on empty. Legacy raw cli ids fall
	// through to the existing path unchanged for backward compatibility.
	//
	// logicalID is the logical id we'll write into the session frame and
	// touch on success. resumeFromLogical is what we pass to drv.Start as
	// ResumeID — taken from the logical's CLISessionID (may be "" for first
	// turn). When logicalID stays "" we use legacy semantics.
	var (
		logicalID         logicalsession.ID
		resumeFromLogical string
		usingLogical      bool
	)
	if s.sessions != nil {
		switch {
		case strings.HasPrefix(requestedSession, "ls-"):
			ls, ok := s.sessions.Get(logicalsession.ID(requestedSession))
			if !ok {
				// Don't silently mint a new logical id — would surprise the
				// user. Emit a streamed error frame so the firmware can
				// surface it the same way it surfaces other turn errors.
				sw, ok := newFinishStreamWriter(w)
				if !ok {
					writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "STREAMING_NOT_SUPPORTED"})
					return
				}
				_ = sw.write(map[string]any{
					"type":  "error",
					"error": "UNKNOWN_LOGICAL_SESSION",
					"text":  "logical session not found: " + requestedSession,
				})
				return
			}
			logicalID = ls.ID
			resumeFromLogical = ls.CLISessionID
			usingLogical = true
			// If the logical session's CLISessionID matches a live cli entry,
			// honor the existing pinning behaviour by treating that as a hit.
			if resumeFromLogical != "" {
				if entry, found := s.agentSessions.get(resumeFromLogical); found {
					if requestedDriver != "" && requestedDriver != entry.driverName {
						writeJSON(w, http.StatusBadRequest, response{
							OK:     false,
							Error:  "SESSION_DRIVER_MISMATCH",
							Detail: "sessionId is pinned to driver=" + entry.driverName + ", request asked for driver=" + requestedDriver,
						})
						return
					}
					pinned, found2 := s.router.Get(entry.driverName)
					if !found2 {
						writeJSON(w, http.StatusInternalServerError, response{
							OK:     false,
							Error:  "UNKNOWN_DRIVER",
							Detail: "session references unregistered driver: " + entry.driverName,
						})
						return
					}
					drv = pinned
					driverName = entry.driverName
					sid = entry.sid
					isNew = false
				}
			}
		case requestedSession == "":
			// Auto-mint a logical session so future turns can come back with
			// its stable id. Cwd defaults to the manager's configured value.
			deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))
			ls, err := s.sessions.Create(deviceID, driverName, "", "")
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, response{
					OK:     false,
					Error:  "CREATE_SESSION_FAILED",
					Detail: err.Error(),
				})
				return
			}
			logicalID = ls.ID
			usingLogical = true
		}
	}

	if !usingLogical && requestedSession != "" {
		// Legacy raw CLI id path (Phase A backward compat).
		if entry, found := s.agentSessions.get(requestedSession); found {
			// Sessions are pinned to the driver that started them. If the
			// caller supplied an explicit driver that disagrees, fail loudly
			// with a 400 instead of silently switching drivers mid-
			// conversation.
			if requestedDriver != "" && requestedDriver != entry.driverName {
				writeJSON(w, http.StatusBadRequest, response{
					OK:     false,
					Error:  "SESSION_DRIVER_MISMATCH",
					Detail: "sessionId is pinned to driver=" + entry.driverName + ", request asked for driver=" + requestedDriver,
				})
				return
			}
			// Ignore any candidate driver from the resolution above and use
			// the pinned one.
			pinned, found2 := s.router.Get(entry.driverName)
			if !found2 {
				writeJSON(w, http.StatusInternalServerError, response{
					OK:     false,
					Error:  "UNKNOWN_DRIVER",
					Detail: "session references unregistered driver: " + entry.driverName,
				})
				return
			}
			drv = pinned
			driverName = entry.driverName
			sid = entry.sid
			isNew = false
		}
	}

	sw, ok := newFinishStreamWriter(w)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "STREAMING_NOT_SUPPORTED"})
		return
	}
	s.metrics.Inc("agent_message_start")
	ctx := r.Context()

	// Outer loop wraps one or two attempts. Attempt 0 resumes the requested
	// session; attempt 1 fires only if the driver emits SESSION_NOT_FOUND
	// (ADR-014) — we mint a fresh session and re-send the user text so the
	// device sees a clean turn instead of a generic AGENT_TURN_FAILED.
	const maxAttempts = 2
	var (
		textCount  int
		errorCount int
		lastError  string
		lastText   string
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		textCount = 0
		errorCount = 0
		lastError = ""
		lastText = ""
		sessionNotFound := false

		if sid == "" {
			// Reaches here on:
			//   1. No sessionId in the request → start a brand-new session.
			//   2. sessionId provided but not in registry (adapter restarted,
			//      picker-loaded, etc.) → must resume so claude-code's
			//      --resume continues the same JSONL.
			//   3. Retry after SESSION_NOT_FOUND (attempt > 0) → no resume.
			startOpts := agent.StartOpts{}
			isResumeAttempt := false
			if attempt == 0 {
				switch {
				case usingLogical:
					// Logical session path: ResumeID comes from the logical's
					// stored CLISessionID (may be "" on first turn — that's
					// fine, drv.Start handles empty resume).
					if resumeFromLogical != "" {
						startOpts.ResumeID = resumeFromLogical
						isResumeAttempt = true
					}
				case requestedSession != "":
					// Legacy raw cli id path.
					startOpts.ResumeID = requestedSession
					isResumeAttempt = true
				}
			}
			newSid, err := drv.Start(s.agentCtx, startOpts)
			if err != nil {
				_ = sw.write(map[string]any{"type": "error", "error": "AGENT_START_FAILED", "detail": err.Error()})
				return
			}
			sid = newSid
			isNew = !isResumeAttempt
			s.agentSessions.put(string(sid), &sessionEntry{
				sid:        sid,
				driverName: driverName,
				lastUsed:   time.Now(),
				state:      "running",
			})
			// Write back the freshly-minted CLI session id to the logical
			// table. Done on every Start (including retry) so the logical's
			// CLISessionID always tracks the live conversation.
			if usingLogical && logicalID != "" {
				if err := s.sessions.UpdateCLISessionID(logicalID, string(sid)); err != nil {
					s.log.Warnf("agent: UpdateCLISessionID logical=%s cli=%s err=%v", logicalID, sid, err)
				}
			}
		} else {
			s.agentSessions.setState(string(sid), "running")
		}

		// Emit the session frame so the client learns (or confirms) the
		// sessionId before any text arrives. On retry, the device sees a
		// second session frame with the new sid+isNew=true — exactly what
		// its NVS needs to update.
		//
		// When the logical-session manager is in play, the device-visible id
		// in the frame is the *logical* id (stable across cli rotation), not
		// the cli sid (which can change on SESSION_NOT_FOUND retry).
		visibleSessionID := string(sid)
		if usingLogical && logicalID != "" {
			visibleSessionID = string(logicalID)
		}
		if err := sw.write(map[string]any{
			"type":      "session",
			"sessionId": visibleSessionID,
			"isNew":     isNew,
			"driver":    driverName,
			"seq":       0,
		}); err != nil {
			s.log.Warnf("agent: write session frame failed: %v", err)
			return
		}
		s.agentSessions.touch(string(sid))

		if attempt == 0 {
			s.log.Infof("phase=agent_start driver=%s sid=%s is_new=%v text_chars=%d", driverName, sid, isNew, len(text))
		} else {
			s.log.Warnf("phase=agent_retry driver=%s sid=%s attempt=%d reason=SESSION_NOT_FOUND",
				driverName, sid, attempt)
		}

		events := drv.Events(sid)
		sendErrCh := make(chan error, 1)
		curSid := sid
		go func() { sendErrCh <- drv.Send(curSid, text) }()

		channelClosed := false
		turnEnded := false
	loop:
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
					channelClosed = true
					break loop
				}
				switch ev.Type {
				case agent.EvText:
					textCount++
					lastText = ev.Text
				case agent.EvError:
					errorCount++
					lastError = ev.Text
					s.agentSessions.setState(string(sid), "error")
					broadcastSID := string(sid)
					if usingLogical && logicalID != "" {
						broadcastSID = string(logicalID)
					}
					s.broadcastSessionStateChange(broadcastSID, "error", ev.Text)
					if strings.HasPrefix(ev.Text, "SESSION_NOT_FOUND") {
						sessionNotFound = true
					}
				case agent.EvTurnEnd:
					turnEnded = true
				}
				// Suppress the SESSION_NOT_FOUND error frame on the attempt
				// we plan to retry — the device should not see a transient
				// error message that we're about to recover from.
				if ev.Type == agent.EvError && sessionNotFound && attempt+1 < maxAttempts {
					// skip emit
				} else if !s.writeAgentEvent(sw, ev) {
					return
				}
				if ev.Type == agent.EvTurnEnd {
					break loop
				}
			}
		}

		if sendErr := <-sendErrCh; sendErr != nil {
			s.log.Errorf("phase=agent_send_failed driver=%s sid=%s err=%v attempt=%d",
				driverName, sid, sendErr, attempt)
		}

		if sessionNotFound && attempt+1 < maxAttempts {
			// Drop the dead session and force a fresh start on next iteration.
			s.agentSessions.mu.Lock()
			delete(s.agentSessions.sessions, string(sid))
			s.agentSessions.mu.Unlock()
			sid = ""
			isNew = false
			s.metrics.Inc("agent_message_session_not_found_retry")
			continue
		}

		// Final attempt: finalize state and notification, then return.
		if !channelClosed {
			s.agentSessions.setState(string(sid), "completed")
			completedSID := string(sid)
			if usingLogical && logicalID != "" {
				completedSID = string(logicalID)
			}
			s.broadcastSessionStateChange(completedSID, "completed", lastText)
		}
		// Bump LastUsedAt on the logical session so the picker can sort by
		// recency. Done after the turn settles (post-retry) so a transient
		// SESSION_NOT_FOUND doesn't double-bump.
		if usingLogical && logicalID != "" && turnEnded {
			if err := s.sessions.Touch(logicalID); err != nil {
				s.log.Warnf("agent: Touch logical=%s err=%v", logicalID, err)
			}
		}
		if turnEnded {
			notifType := "turn_end"
			if errorCount > 0 && textCount == 0 {
				notifType = "error"
			}
			notifSID := string(sid)
			if usingLogical && logicalID != "" {
				notifSID = string(logicalID)
			}
			s.pushNotification(SessionNotification{
				SessionID: notifSID,
				Driver:    driverName,
				Type:      notifType,
				Preview:   lastText,
			})
		}
		if errorCount > 0 && textCount == 0 {
			s.metrics.Inc("agent_message_error_only")
			s.log.Warnf("phase=agent_done_error_only driver=%s sid=%s errors=%d last=%q",
				driverName, sid, errorCount, lastError)
		} else {
			s.metrics.Inc("agent_message_ok")
			s.log.Infof("phase=agent_done driver=%s sid=%s text=%d errors=%d",
				driverName, sid, textCount, errorCount)
		}
		return
	}
}

// agentSessionCreateRequest is the body for POST /v1/agent/sessions.
// Per ADR-014 the device never picks the cwd — it's preconfigured in the
// adapter's `BBCLAW_DEFAULT_CWD` (or, future: a named cwd pool selected by
// the cloud admin console).
type agentSessionCreateRequest struct {
	Driver string `json:"driver,omitempty"`
	Title  string `json:"title,omitempty"`
	Cwd    string `json:"cwd,omitempty"` // optional override; defaults to manager's default
}

// handleAgentSessionCreate mints a new logical session (ADR-014).
//
//	POST /v1/agent/sessions
//	{"driver":"claude-code","title":"...","cwd":"..."}
//	→ {"ok":true,"data":{"session":{...}}}
//
// The CLI conversation is NOT spawned here — that happens lazily on the
// first /v1/agent/message turn referencing this session. This keeps "new
// session" UX cheap and avoids spawning subprocesses we may never need.
func (s *Server) handleAgentSessionCreate(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	if s.sessions == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
		return
	}

	var req agentSessionCreateRequest
	if r.Body != nil {
		// Empty body is fine — defaults will apply.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	driver := strings.TrimSpace(req.Driver)
	if driver == "" {
		if d := s.router.Default(); d != nil {
			driver = d.Name()
		} else {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "DRIVER_REQUIRED"})
			return
		}
	}
	if _, ok := s.router.Get(driver); !ok {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: driver})
		return
	}

	deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))
	sess, err := s.sessions.Create(deviceID, driver, strings.TrimSpace(req.Cwd), strings.TrimSpace(req.Title))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "CREATE_SESSION_FAILED", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"session": sess}})
}

// handleAgentSessionsLogical lists rows from the logical-session manager
// (ADR-014). Filters: deviceId (empty matches any), driver (empty matches
// any). Limit defaults to 50, capped at 200.
func (s *Server) handleAgentSessionsLogical(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
		return
	}
	deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))
	driverName := strings.TrimSpace(r.URL.Query().Get("driver"))
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	sessions := s.sessions.List(deviceID, driverName, limit)
	if sessions == nil {
		sessions = []*logicalsession.LogicalSession{}
	}
	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: map[string]any{"sessions": sessions},
	})
}

// agentSessionUpdateRequest is the body for PATCH /v1/agent/sessions/{id}.
// Both fields are optional pointers so we can distinguish "not present" from
// "present but empty". Empty cwd clears the field per Manager.UpdateCwd.
type agentSessionUpdateRequest struct {
	Title *string `json:"title,omitempty"`
	Cwd   *string `json:"cwd,omitempty"`
}

// handleAgentSessionUpdate applies a partial update to a logical session
// (ADR-014). Title and cwd are independently optional; an entirely empty
// patch is a 400.
//
//	PATCH /v1/agent/sessions/{id}
//	{"title":"...","cwd":"..."}
//	→ {"ok":true,"data":{"session":{...}}}
func (s *Server) handleAgentSessionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	if s.sessions == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "LOGICAL_SESSIONS_DISABLED"})
		return
	}
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SESSION_ID_REQUIRED"})
		return
	}
	if !strings.HasPrefix(sessionID, "ls-") {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "NOT_LOGICAL", Detail: "id must have ls- prefix"})
		return
	}

	var req agentSessionUpdateRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST"})
			return
		}
	}
	if req.Title == nil && req.Cwd == nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_PATCH"})
		return
	}

	if _, ok := s.sessions.Get(logicalsession.ID(sessionID)); !ok {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
		return
	}
	if req.Title != nil {
		if err := s.sessions.SetTitle(logicalsession.ID(sessionID), *req.Title); err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "UPDATE_SESSION_FAILED", Detail: err.Error()})
			return
		}
	}
	if req.Cwd != nil {
		if err := s.sessions.UpdateCwd(logicalsession.ID(sessionID), *req.Cwd); err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "UPDATE_SESSION_FAILED", Detail: err.Error()})
			return
		}
	}

	updated, ok := s.sessions.Get(logicalsession.ID(sessionID))
	if !ok {
		// Race against Delete; surface the same shape as the up-front check.
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
		return
	}
	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: map[string]any{"session": updated},
	})
}

// handleAgentDeleteSession removes a session from the registry and stops it.
// When the id has the "ls-" prefix it's also dropped from the logical-session
// table; otherwise we treat it as a CLI-native id (ADR-014 phase A backward-
// compat for legacy firmware that still sends raw cli ids).
//
//	DELETE /v1/agent/sessions/{id}
func (s *Server) handleAgentDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.router == nil || s.agentSessions == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "SESSION_ID_REQUIRED"})
		return
	}

	// Logical session path (ADR-014).
	if s.sessions != nil && strings.HasPrefix(sessionID, "ls-") {
		ls, ok := s.sessions.Get(logicalsession.ID(sessionID))
		if !ok {
			writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
			return
		}
		// Best-effort tear down of the underlying CLI conversation if we
		// know it. Stop is idempotent across drivers.
		if ls.CLISessionID != "" {
			if drv, ok := s.router.Get(ls.Driver); ok {
				_ = drv.Stop(agent.SessionID(ls.CLISessionID))
			}
			s.agentSessions.mu.Lock()
			delete(s.agentSessions.sessions, ls.CLISessionID)
			s.agentSessions.mu.Unlock()
		}
		if err := s.sessions.Delete(ls.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "DELETE_SESSION_FAILED", Detail: err.Error()})
			return
		}
		s.log.Infof("agent: deleted logical session=%s driver=%s cli=%s", ls.ID, ls.Driver, ls.CLISessionID)
		writeJSON(w, http.StatusOK, response{OK: true})
		return
	}

	s.agentSessions.mu.Lock()
	entry, found := s.agentSessions.sessions[sessionID]
	if found {
		delete(s.agentSessions.sessions, sessionID)
	}
	s.agentSessions.mu.Unlock()

	if !found {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "SESSION_NOT_FOUND"})
		return
	}

	if drv, ok := s.router.Get(entry.driverName); ok {
		if err := drv.Stop(entry.sid); err != nil {
			s.log.Warnf("agent: delete session stop failed driver=%s sid=%s err=%v", entry.driverName, entry.sid, err)
		}
	}
	s.log.Infof("agent: deleted session=%s driver=%s", sessionID, entry.driverName)
	writeJSON(w, http.StatusOK, response{OK: true})
}

// broadcastSessionStateChange emits a session.state_change WebSocket event to
// all connected clients. It is a no-op when the hub is nil (no WS clients).
//
// Payload shape:
//
//	{"type":"event","kind":"session.state_change",
//	 "payload":{"sessionId":"ls-xxx","state":"completed","preview":"..."}}
func (s *Server) broadcastSessionStateChange(sessionID, state, preview string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(map[string]any{
		"type": "event",
		"kind": "session.state_change",
		"payload": map[string]any{
			"sessionId": sessionID,
			"state":     state,
			"preview":   truncatePreview(preview, 48),
		},
	})
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
