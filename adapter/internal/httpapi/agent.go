package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/voicecmd"
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

// drop removes id from the registry. Safe to call on missing ids. Equivalent
// to the inline mu.Lock + delete the handler used previously.
func (r *sessionRegistry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
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

// SetDriverState attaches the persistent driver-preference store. When set,
// the router's "default" driver is overridden by store.ActiveDriver() at
// session-create time, and StartOpts.Model is populated from
// store.ActiveModel(driver). nil disables the override path entirely (the
// router's first-registered-wins default applies as before).
func (s *Server) SetDriverState(store *driverstate.Store) { s.driverState = store }

// resolveActiveDriver picks the driver name to use when the request didn't
// specify one. Priority: 1) persisted driverState.ActiveDriver, 2) router's
// own default. Both fall back gracefully to "" when neither resolves.
func (s *Server) resolveActiveDriver() string {
	if s.driverState != nil {
		if name := s.driverState.ActiveDriver(); name != "" {
			if _, ok := s.router.Get(name); ok {
				return name
			}
			// Persisted driver no longer registered (operator removed it from
			// AGENT_ENABLED_DRIVERS). Fall back to router default.
			s.log.Warnf("driverstate: active_driver=%q not registered, falling back to router default", name)
		}
	}
	if d := s.router.Default(); d != nil {
		return d.Name()
	}
	return ""
}

// resolveActiveModel returns the persisted active model for driver, or ""
// when the store is unset / no override exists. The driver decides what ""
// means (typically "use the driver's built-in default").
func (s *Server) resolveActiveModel(driver string) string {
	if s.driverState == nil || driver == "" {
		return ""
	}
	return s.driverState.ActiveModel(driver)
}

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
	// Wire the server as the session resolver so the router's SendSlashCommand
	// can stop/reset live sessions for Agent Bus drivers (issue #53).
	r.SetSessionResolver(s)
}

// ResolveSession implements agent.SessionResolver. It maps a device-visible
// session key (logical "ls-" id or raw CLI id) to the live (driverName,
// SessionID) pair held in the session registry.
//
// For logical ids the CLISessionID stored in the logical-session manager is
// used to look up the registry entry. For raw CLI ids the registry is
// consulted directly.
func (s *Server) ResolveSession(sessionKey string) (driverName string, sid agent.SessionID, ok bool) {
	if s.agentSessions == nil {
		return "", "", false
	}
	// Logical session path: resolve ls- id → CLI session id → registry entry.
	if s.sessions != nil && strings.HasPrefix(sessionKey, "ls-") {
		ls, found := s.sessions.Get(logicalsession.ID(sessionKey))
		if found && ls.CLISessionID != "" {
			if entry, found2 := s.agentSessions.get(ls.CLISessionID); found2 {
				return entry.driverName, entry.sid, true
			}
		}
		return "", "", false
	}
	// Raw CLI id path.
	if entry, found := s.agentSessions.get(sessionKey); found {
		return entry.driverName, entry.sid, true
	}
	return "", "", false
}

// ResetSession implements agent.SessionResolver. It clears the live session
// binding for the given key so the next agent turn starts a fresh CLI
// conversation. For logical ids the CLISessionID is cleared in the manager;
// for raw CLI ids the registry entry is removed.
func (s *Server) ResetSession(sessionKey string) {
	if s.agentSessions == nil {
		return
	}
	// Logical session path.
	if s.sessions != nil && strings.HasPrefix(sessionKey, "ls-") {
		ls, found := s.sessions.Get(logicalsession.ID(sessionKey))
		if found {
			// Remove the live registry entry so the next turn starts fresh.
			if ls.CLISessionID != "" {
				s.agentSessions.mu.Lock()
				delete(s.agentSessions.sessions, ls.CLISessionID)
				s.agentSessions.mu.Unlock()
			}
			// Clear the stored CLI session id so drv.Start won't try to resume it.
			if err := s.sessions.UpdateCLISessionID(ls.ID, ""); err != nil {
				s.log.Warnf("agent: ResetSession clear cli id logical=%s err=%v", ls.ID, err)
			}
		}
		return
	}
	// Raw CLI id path.
	s.agentSessions.mu.Lock()
	delete(s.agentSessions.sessions, sessionKey)
	s.agentSessions.mu.Unlock()
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

// driverInfoExtended is the device-facing shape of one driver row.
// Extends agent.DriverInfo with the per-driver model list and the
// currently-active model id (when the driverstate store is wired).
type driverInfoExtended struct {
	Name         string             `json:"name"`
	Capabilities agent.Capabilities `json:"capabilities"`
	Models       []agent.ModelInfo  `json:"models,omitempty"`
	ActiveModel  string             `json:"active_model,omitempty"`
}

// handleAgentDrivers lists the drivers currently registered on the router,
// each augmented with the model catalog from agent.ModelLister (when the
// driver implements it) and the currently-active model id from driverState.
//
//	GET /v1/agent/drivers
//	response:
//	  {"ok":true,"data":{
//	    "active_driver":"claude-code",
//	    "drivers":[
//	      {"name":"claude-code","capabilities":{...},
//	       "models":[{"id":"...","label":"..."},...],
//	       "active_model":"claude-sonnet-4-6"},
//	      ...
//	    ]
//	  }}
//
// `models` is omitted (not just empty) for drivers that don't implement
// ModelLister so the device UI can hide the Model row entirely for them
// (e.g. openclaw). `active_model` is omitted when no override is persisted.
func (s *Server) handleAgentDrivers(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	base := s.router.List()
	// Stable order makes the response testable and easier to eyeball.
	sort.Slice(base, func(i, j int) bool { return base[i].Name < base[j].Name })

	out := make([]driverInfoExtended, 0, len(base))
	for _, info := range base {
		row := driverInfoExtended{
			Name:         info.Name,
			Capabilities: info.Capabilities,
			ActiveModel:  s.resolveActiveModel(info.Name),
		}
		if drv, ok := s.router.Get(info.Name); ok {
			if ml, isLister := drv.(agent.ModelLister); isLister {
				if models, err := ml.ListModels(r.Context()); err == nil {
					row.Models = models
				} else {
					s.log.Warnf("driver %s: ListModels failed: %v", info.Name, err)
				}
			}
		}
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, response{
		OK: true,
		Data: map[string]any{
			"active_driver": s.resolveActiveDriver(),
			"drivers":       out,
		},
	})
}

// handleAgentActiveDriverPut persists the active driver selection.
//
//	PUT /v1/agent/active_driver  {"name":"opencode"}
//	→ {"ok":true,"data":{"active_driver":"opencode"}}
//
// 400 UNKNOWN_DRIVER if name is not a registered driver. 501
// DRIVERSTATE_NOT_CONFIGURED when the store wasn't wired (operator
// disabled persistence via env). The router's runtime default is updated
// in lock-step so /v1/agent/message without an explicit driver also picks
// the new selection immediately.
func (s *Server) handleAgentActiveDriverPut(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	if s.driverState == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "DRIVERSTATE_NOT_CONFIGURED"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_NAME"})
		return
	}
	if _, ok := s.router.Get(name); !ok {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: name})
		return
	}
	if err := s.driverState.SetActiveDriver(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "PERSIST_FAILED", Detail: err.Error()})
		return
	}
	// Mirror into the router so /v1/agent/message without explicit driver
	// resolves to the new selection without another round-trip.
	s.router.SetDefault(name)
	s.log.Infof("driverstate: active_driver set to %q", name)
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"active_driver": name}})
}

// handleAgentActiveModelPut persists the active model for one driver.
//
//	PUT /v1/agent/drivers/{name}/active_model  {"model":"claude-opus-4-7"}
//	→ {"ok":true,"data":{"driver":"claude-code","active_model":"claude-opus-4-7"}}
//
// Passing model="" clears the persisted override (the driver falls back to
// its built-in default). 400 UNKNOWN_DRIVER if {name} isn't registered.
// We DO NOT validate the model id against ListModels — letting the operator
// configure a model id that ListModels doesn't enumerate (e.g. a model only
// reachable via OLLAMA_BASE_URL the device hasn't seen yet) is intentional.
func (s *Server) handleAgentActiveModelPut(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "AGENT_NOT_CONFIGURED"})
		return
	}
	if s.driverState == nil {
		writeJSON(w, http.StatusNotImplemented, response{OK: false, Error: "DRIVERSTATE_NOT_CONFIGURED"})
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "EMPTY_NAME"})
		return
	}
	if _, ok := s.router.Get(name); !ok {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_DRIVER", Detail: name})
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "INVALID_REQUEST", Detail: err.Error()})
		return
	}
	model := strings.TrimSpace(body.Model)
	if err := s.driverState.SetActiveModel(name, model); err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "PERSIST_FAILED", Detail: err.Error()})
		return
	}
	s.log.Infof("driverstate: %s active_model set to %q", name, model)
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{
		"driver":       name,
		"active_model": model,
	}})
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

	// Voice command interception: if the transcript matches a slash command
	// (e.g. "停止" → /stop, "新对话" → /new, "状态" → /status), dispatch it
	// directly to the router and return a confirmation — no agent turn needed.
	// This mirrors the pipeline.Wrap interception used on the stream/finish
	// path and fixes silent command drops for Agent Bus drivers (issue #53).
	if vcmd := voicecmd.Match(text); vcmd != nil {
		sessionKey := strings.TrimSpace(req.SessionId)
		s.log.Infof("agent: voice_command cmd=%s session=%s", vcmd.Command, sessionKey)
		reply, err := s.router.SendSlashCommand(r.Context(), vcmd.Command, sessionKey)
		if err != nil {
			s.log.Errorf("agent: voice_command failed cmd=%s err=%v", vcmd.Command, err)
		}
		if reply == "" {
			switch vcmd.Command {
			case "/stop":
				reply = "已停止"
			case "/new":
				reply = "新对话已开始"
			case "/status":
				reply = "状态已查询"
			default:
				reply = "已执行"
			}
		}
		sw, ok := newFinishStreamWriter(w)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "STREAMING_NOT_SUPPORTED"})
			return
		}
		_ = sw.write(map[string]any{"type": "text", "text": reply, "seq": 0})
		_ = sw.write(map[string]any{"type": string(agent.EvTurnEnd), "seq": 1})
		return
	}

	requestedSession := strings.TrimSpace(req.SessionId)
	deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))

	// Lazily-built stream writer. The parse phase may return a PreStream
	// CodedError before any frame is written, in which case we reply with a
	// plain JSON 4xx. Once RunTurn touches the Sink the response is committed
	// to NDJSON and errors are surfaced as streamed frames instead.
	sink := &localEventSink{s: s, w: w}

	eng := butler.NewEngine(butler.Deps{
		Router:   s.router,
		Sessions: s.sessions,
		Registry: &localSessionRegistry{r: s.agentSessions},
		Sink:     sink,
		Hooks: butler.Hooks{
			OnStateChange: func(visibleID, state, preview string) {
				s.broadcastSessionStateChange(visibleID, state, preview)
			},
			OnTurnComplete: func(n butler.Notification) {
				s.pushNotification(SessionNotification{
					SessionID: n.SessionID,
					Driver:    n.Driver,
					Type:      n.Type,
					Preview:   n.Preview,
				})
			},
			OnFinalReply: nil,
		},
		Policy: butler.Policy{
			ReuseWindow:          s.cfg.SessionReuseWindow,
			AllowBareCLIID:       false,
			AutoTitle:            true,
			EmitTurnEndFrame:     true,
			EmitStartFailedFrame: true,
			MaxAttempts:          2,
		},
		Metrics:            &localMetrics{m: s.metrics},
		Log:                s.log,
		ResolveActiveModel: s.resolveActiveModel,
		StartCtx:           s.agentCtx,
	})

	_, runErr := eng.RunTurn(r.Context(), butler.Request{
		Text:             text,
		RequestedDriver:  requestedDriver,
		RequestedSession: requestedSession,
		DeviceID:         deviceID,
	})
	if runErr == nil {
		return
	}
	var ce *butler.CodedError
	if errors.As(runErr, &ce) {
		if ce.PreStream {
			// Parse-phase failure: RunTurn guarantees no frame was emitted, so
			// we can still reply with a plain JSON 4xx/5xx.
			writeJSON(w, localStatusFor(ce), response{OK: false, Error: localCodeFor(ce), Detail: ce.Detail})
			return
		}
		// Runtime/streamed error (UNKNOWN_LOGICAL_SESSION / AGENT_START_FAILED):
		// butler already emitted the streamed error frame via the Sink. Nothing
		// further to write.
		return
	}
	// ctx cancellation (client gone) or other non-coded error: stream already
	// committed, so there's nothing more to send.
}

// localStatusFor maps a butler CodedError to the LAN-direct HTTP status,
// preserving the historical responses (SESSION_UNREGISTERED_DRIVER → 500).
func localStatusFor(ce *butler.CodedError) int {
	if ce.HTTPStatus != 0 {
		return ce.HTTPStatus
	}
	return http.StatusBadRequest
}

// localCodeFor maps a butler internal Code to the LAN-direct response error
// string. The only divergence from the internal code is
// SESSION_UNREGISTERED_DRIVER, which LOCAL historically reports as
// UNKNOWN_DRIVER with a 500.
func localCodeFor(ce *butler.CodedError) string {
	if ce.Code == "SESSION_UNREGISTERED_DRIVER" {
		return "UNKNOWN_DRIVER"
	}
	return ce.Code
}

// localEventSink adapts the NDJSON finishStreamWriter to butler.EventSink. The
// stream writer is created lazily on first emit so the parse phase can still
// reply with a plain JSON 4xx when no frame has been written yet. butler only
// calls EventSink methods after the parse phase has cleared all PreStream
// errors, so the first emit safely commits the response to NDJSON.
type localEventSink struct {
	s  *Server
	w  http.ResponseWriter
	sw *finishStreamWriter
}

// ensure builds the stream writer on first use. Returns false only when the
// ResponseWriter doesn't support flushing (mirrors STREAMING_NOT_SUPPORTED).
func (l *localEventSink) ensure() bool {
	if l.sw != nil {
		return true
	}
	sw, ok := newFinishStreamWriter(l.w)
	if !ok {
		writeJSON(l.w, http.StatusInternalServerError, response{OK: false, Error: "STREAMING_NOT_SUPPORTED"})
		return false
	}
	l.sw = sw
	return true
}

func (l *localEventSink) EmitSession(visibleID string, isNew bool, driver string) bool {
	if !l.ensure() {
		return false
	}
	if err := l.sw.write(map[string]any{
		"type":      "session",
		"sessionId": visibleID,
		"isNew":     isNew,
		"driver":    driver,
		"seq":       0,
	}); err != nil {
		l.s.log.Warnf("agent: write session frame failed: %v", err)
		return false
	}
	return true
}

func (l *localEventSink) EmitEvent(ev agent.Event) bool {
	if !l.ensure() {
		return false
	}
	return l.s.writeAgentEvent(l.sw, ev)
}

func (l *localEventSink) EmitError(code, text string, detailField bool) bool {
	if !l.ensure() {
		return false
	}
	frame := map[string]any{"type": "error", "error": code}
	if detailField {
		frame["detail"] = text
	} else {
		frame["text"] = text
	}
	return l.sw.write(frame) == nil
}

// localSessionRegistry adapts *sessionRegistry to butler.SessionRegistry.
type localSessionRegistry struct{ r *sessionRegistry }

func (a *localSessionRegistry) Get(id string) (string, agent.SessionID, bool) {
	e, ok := a.r.get(id)
	if !ok {
		return "", "", false
	}
	return e.driverName, e.sid, true
}

func (a *localSessionRegistry) Put(id string, driverName string, sid agent.SessionID) {
	a.r.put(id, &sessionEntry{sid: sid, driverName: driverName, lastUsed: time.Now(), state: "running"})
}

func (a *localSessionRegistry) Touch(id string)           { a.r.touch(id) }
func (a *localSessionRegistry) Drop(id string)            { a.r.drop(id) }
func (a *localSessionRegistry) SetState(id, state string) { a.r.setState(id, state) }

// localMetrics maps butler 的语义化指标事件到 LAN-direct 的历史计数器名,逐字复刻
// 原 handleAgentMessage 的指标(LOCAL 成功分支用 agent_message_ok 与
// agent_message_error_only,后者条件为 errorCount>0 && textCount==0)。
type localMetrics struct{ m *obs.Metrics }

func (l *localMetrics) TurnStart()            { l.m.Inc("agent_message_start") }
func (l *localMetrics) ResumeSkippedMissing() { l.m.Inc("agent_message_resume_skipped_missing") }
func (l *localMetrics) SessionNotFoundRetry() { l.m.Inc("agent_message_session_not_found_retry") }
func (l *localMetrics) TurnDone(turnEnded bool, textCount, errorCount int) {
	if errorCount > 0 && textCount == 0 {
		l.m.Inc("agent_message_error_only")
	} else {
		l.m.Inc("agent_message_ok")
	}
}

// agentSessionCreateRequest is the body for POST /v1/agent/sessions.
// Per ADR-014 the device never picks the cwd — it's preconfigured in the
// adapter's `BBCLAW_DEFAULT_CWD` (or, future: a named cwd pool selected by
// the cloud admin console).
type agentSessionCreateRequest struct {
	Driver  string `json:"driver,omitempty"`
	Title   string `json:"title,omitempty"`
	Cwd     string `json:"cwd,omitempty"`     // optional override; defaults to manager's default
	CwdName string `json:"cwdName,omitempty"` // issue #30: select cwd by pool name
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
	// Resolve cwd: cwdName takes priority over raw cwd field.
	cwd := strings.TrimSpace(req.Cwd)
	if cwdName := strings.TrimSpace(req.CwdName); cwdName != "" {
		if resolved, ok := s.resolveCwdByName(cwdName); ok {
			cwd = resolved
		} else {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "UNKNOWN_CWD_NAME", Detail: cwdName})
			return
		}
	}
	sess, err := s.sessions.Create(deviceID, driver, cwd, strings.TrimSpace(req.Title))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "CREATE_SESSION_FAILED", Detail: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"session": sess}})
}

// handleAgentSessionsLogical lists rows from the logical-session manager
// (ADR-014). Filters: deviceId (empty matches any), driver (empty matches
// any). Limit defaults to 50, capped at 200. Sessions older than
// SessionMaxAge are excluded from the response.
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
	// T4: Filter out expired sessions so the picker doesn't show stale entries.
	if s.cfg.SessionMaxAge > 0 {
		cutoff := time.Now().Add(-s.cfg.SessionMaxAge)
		filtered := sessions[:0]
		for _, sess := range sessions {
			if sess.LastUsedAt.After(cutoff) {
				filtered = append(filtered, sess)
			}
		}
		sessions = filtered
	}
	// Populate CwdName for each session: reverse-lookup path→name in CwdPool;
	// fall back to filepath.Base(cwd) when no pool entry matches.
	// We copy each session into a local struct so we don't mutate the stored
	// LogicalSession (CwdName is an outbound-only field, never persisted).
	type sessionWithName struct {
		logicalsession.LogicalSession
		CwdName string `json:"cwdName,omitempty"`
	}
	out := make([]sessionWithName, len(sessions))
	for i, sess := range sessions {
		out[i].LogicalSession = *sess
		if sess.Cwd != "" {
			name := filepath.Base(sess.Cwd) // default: basename
			for _, entry := range s.cfg.CwdPool {
				if entry.Path == sess.Cwd {
					name = entry.Name
					break
				}
			}
			out[i].CwdName = name
		}
	}
	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: map[string]any{"sessions": out},
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

// resolveCwdByName looks up a cwd path by name in the configured pool.
// Returns ("", false) when the name is not found.
func (s *Server) resolveCwdByName(name string) (string, bool) {
	for _, entry := range s.cfg.CwdPool {
		if entry.Name == name {
			return entry.Path, true
		}
	}
	return "", false
}

// handleAgentCwdPool returns the configured CWD pool entries.
//
//	GET /v1/agent/cwd-pool
//	response: {"ok":true,"data":{"pool":[{"name":"myproject"},{"name":"side"}]}}
//
// Only the name is returned to the device — the full filesystem path is not
// sent over the wire (it leaks host info and the device only needs the name
// to pass back as cwdName in POST /v1/agent/sessions).
func (s *Server) handleAgentCwdPool(w http.ResponseWriter, r *http.Request) {
	type poolItem struct {
		Name string `json:"name"`
	}
	items := make([]poolItem, 0, len(s.cfg.CwdPool))
	for _, e := range s.cfg.CwdPool {
		items = append(items, poolItem{Name: e.Name})
	}
	writeJSON(w, http.StatusOK, response{
		OK:   true,
		Data: map[string]any{"pool": items},
	})
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
	case agent.EvSessionInit:
		// Internal event — not forwarded to the device.
		return true
	}
	if err := sw.write(frame); err != nil {
		s.log.Warnf("agent: write frame failed: %v", err)
		return false
	}
	return true
}
