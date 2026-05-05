package homeadapter

// Phase 4.8 — cloud Agent Bus proxy.
//
// The cloud-side reverse proxy (cloud/internal/httpapi/agent_proxy.go) tunnels
// device HTTP requests on /v1/agent/{drivers,message} through the existing
// device→cloud→home_adapter WebSocket relay. This file implements the
// home-adapter side of that tunnel: when an `agent.drivers` or `agent.message`
// request envelope arrives, dispatch it directly into the locally-attached
// agent.Router and stream the resulting NDJSON-shaped events back as `event`
// envelopes followed by a final `reply` envelope.
//
// Why not reuse `chat.text` / `chat.drivers`? Those flatten everything to a
// single `voice.reply.delta` stream and drop session/tokens/turn_end frames
// that the firmware Agent Chat UI relies on. We need a higher-fidelity
// transport that preserves the agent.Event schema verbatim.
//
// Design notes:
//   - Sessions are kept alive across cloud requests in a process-local
//     registry, mirroring the httpapi.Server behaviour. Idle sessions are
//     swept out by the same TTL the local httpapi serves direct LAN requests
//     under (see agentSessionTTL below).
//   - We do NOT reach into httpapi.Server because (a) it would create a
//     cyclic dependency and (b) the router is already injected here via
//     SetRouter, so we have everything we need.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// agentSessionTTL must mirror httpapi.sessionTTL so the user sees the same
// behaviour whether they're on local_home (direct LAN) or cloud_saas
// (proxied) transport. If you bump one, bump the other.
const agentSessionTTL = 30 * time.Minute

// agentProxySession pins one persistent agent session to the driver that
// owns it. Looked up by the device-visible session id (string form of
// agent.SessionID).
type agentProxySession struct {
	sid        agent.SessionID
	driverName string
	lastUsed   time.Time
}

// agentProxyRegistry is a tiny goroutine-safe map; the higher-level
// homeadapter.Adapter holds one of these and the cloud-relay request loop
// reaches into it to resume sessions across turns.
type agentProxyRegistry struct {
	mu       sync.Mutex
	sessions map[string]*agentProxySession
}

func newAgentProxyRegistry() *agentProxyRegistry {
	return &agentProxyRegistry{sessions: make(map[string]*agentProxySession)}
}

func (r *agentProxyRegistry) get(id string) (*agentProxySession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[id]
	return e, ok
}

func (r *agentProxyRegistry) put(id string, e *agentProxySession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = e
}

func (r *agentProxyRegistry) touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[id]; ok {
		e.lastUsed = time.Now()
	}
}

func (r *agentProxyRegistry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

// handleAgentSessionsRequest lists sessions for a driver, proxied from cloud.
func (a *Adapter) handleAgentSessionsRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	if a.router == nil {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.reply",
			Payload:    map[string]any{"sessions": []any{}},
		})
	}
	driverName, _ := env.Payload["driver"].(string)
	driverName = strings.TrimSpace(driverName)
	if driverName == "" {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.reply",
			Payload:    map[string]any{"error": "DRIVER_REQUIRED"},
		})
	}
	drv, ok := a.router.Get(driverName)
	if !ok {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.reply",
			Payload:    map[string]any{"error": "UNKNOWN_DRIVER"},
		})
	}
	lister, ok := drv.(agent.SessionLister)
	if !ok {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.reply",
			Payload:    map[string]any{"sessions": []any{}},
		})
	}
	limit := 6
	if l, ok := env.Payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	sessions, err := lister.ListSessions(context.Background(), limit)
	if err != nil {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.reply",
			Payload:    map[string]any{"error": "LIST_SESSIONS_FAILED", "detail": err.Error()},
		})
	}
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "agent.sessions.reply",
		Payload:    map[string]any{"sessions": sessions},
	})
}

// handleAgentSessionsCreateRequest mints a logical session via the
// home-adapter's local manager when the cloud proxies a firmware
// `POST /v1/agent/sessions` (ADR-014 phase B). The cloud sends:
//
//	{type:"request", kind:"agent.sessions.create",
//	 payload:{driver, title?, cwd?, deviceId}}
//
// We reply with kind="agent.sessions.create.reply" and payload
// {session:{id, driver, cwd, title, createdAt, lastUsedAt}} on success
// or {error, detail} on failure. The CLI conversation is NOT spawned
// here — it's lazily created on the first agent.message turn that
// references this logical id.
func (a *Adapter) handleAgentSessionsCreateRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(payload map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.create.reply",
			Payload:    payload,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	if a.sessions == nil {
		return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
	}

	driver, _ := env.Payload["driver"].(string)
	driver = strings.TrimSpace(driver)
	if driver == "" {
		if d := a.router.Default(); d != nil {
			driver = d.Name()
		} else {
			return reply(map[string]any{"error": "DRIVER_REQUIRED"})
		}
	}
	if _, ok := a.router.Get(driver); !ok {
		return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": driver})
	}

	title, _ := env.Payload["title"].(string)
	cwd, _ := env.Payload["cwd"].(string)
	cwdName, _ := env.Payload["cwdName"].(string)
	deviceID, _ := env.Payload["deviceId"].(string)
	if deviceID == "" {
		// Cloud should always thread this through; fall back to envelope's
		// own DeviceID rather than refusing — keeps the path forgiving.
		deviceID = env.DeviceID
	}

	// Resolve cwd: cwdName takes priority over raw cwd field (issue #30).
	resolvedCwd := strings.TrimSpace(cwd)
	if name := strings.TrimSpace(cwdName); name != "" {
		if path, ok := a.resolveCwdByName(name); ok {
			resolvedCwd = path
		} else {
			return reply(map[string]any{"error": "UNKNOWN_CWD_NAME", "detail": name})
		}
	}

	sess, err := a.sessions.Create(strings.TrimSpace(deviceID), driver, resolvedCwd, strings.TrimSpace(title))
	if err != nil {
		return reply(map[string]any{"error": "CREATE_SESSION_FAILED", "detail": err.Error()})
	}
	return reply(map[string]any{"session": sess})
}

// handleAgentSessionsUpdateRequest applies a partial update to a logical
// session via the cloud relay (ADR-014). Cloud sends:
//
//	{type:"request", kind:"agent.sessions.update",
//	 payload:{sessionId, title?, cwd?}}
//
// Reply kind="agent.sessions.update.reply" with {session:{...}} on success
// or {error, detail} on failure. Validation matches the LAN-direct PATCH:
// missing id, non-ls id, empty patch, and unknown id all surface as their
// own error codes.
func (a *Adapter) handleAgentSessionsUpdateRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(payload map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.update.reply",
			Payload:    payload,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	if a.sessions == nil {
		return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
	}
	sid, _ := env.Payload["sessionId"].(string)
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return reply(map[string]any{"error": "SESSION_ID_REQUIRED"})
	}
	if !strings.HasPrefix(sid, "ls-") {
		return reply(map[string]any{"error": "NOT_LOGICAL", "detail": "id must have ls- prefix"})
	}

	titleRaw, hasTitle := env.Payload["title"]
	cwdRaw, hasCwd := env.Payload["cwd"]
	if !hasTitle && !hasCwd {
		return reply(map[string]any{"error": "EMPTY_PATCH"})
	}

	if _, ok := a.sessions.Get(logicalsession.ID(sid)); !ok {
		return reply(map[string]any{"error": "SESSION_NOT_FOUND"})
	}
	if hasTitle {
		title, _ := titleRaw.(string)
		if err := a.sessions.SetTitle(logicalsession.ID(sid), title); err != nil {
			return reply(map[string]any{"error": "UPDATE_SESSION_FAILED", "detail": err.Error()})
		}
	}
	if hasCwd {
		cwd, _ := cwdRaw.(string)
		if err := a.sessions.UpdateCwd(logicalsession.ID(sid), cwd); err != nil {
			return reply(map[string]any{"error": "UPDATE_SESSION_FAILED", "detail": err.Error()})
		}
	}
	updated, ok := a.sessions.Get(logicalsession.ID(sid))
	if !ok {
		return reply(map[string]any{"error": "SESSION_NOT_FOUND"})
	}
	return reply(map[string]any{"session": updated})
}

// handleAgentSessionsListLogicalRequest lists logical sessions via the cloud
// relay (ADR-014). Cloud sends:
//
//	{type:"request", kind:"agent.sessions.list.logical",
//	 payload:{deviceId?, driver?, limit?}}
//
// Reply kind="agent.sessions.list.logical.reply" with {sessions:[...]} on
// success or {error, detail} on failure. Filters mirror the LAN-direct
// kind=logical query: empty deviceId/driver matches all.
func (a *Adapter) handleAgentSessionsListLogicalRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(payload map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.list.logical.reply",
			Payload:    payload,
		})
	}
	if a.sessions == nil {
		return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
	}
	deviceID, _ := env.Payload["deviceId"].(string)
	deviceID = strings.TrimSpace(deviceID)
	driverName, _ := env.Payload["driver"].(string)
	driverName = strings.TrimSpace(driverName)
	limit := 50
	if l, ok := env.Payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if limit > 200 {
		limit = 200
	}
	sessions := a.sessions.List(deviceID, driverName, limit)
	if sessions == nil {
		sessions = []*logicalsession.LogicalSession{}
	}
	return reply(map[string]any{"sessions": sessions})
}

// handleAgentSessionsDeleteRequest removes a logical session via the cloud
// relay (ADR-014 admin). Cloud sends:
//
//	{type:"request", kind:"agent.sessions.delete",
//	 payload:{sessionId}}
//
// Reply kind="agent.sessions.delete.reply" with {ok:true} on success or
// {error, detail} on failure. Mirrors the LAN-direct DELETE handler: only
// "ls-" prefixed ids are accepted; the underlying CLI conversation (if any)
// is best-effort stopped via Driver.Stop.
func (a *Adapter) handleAgentSessionsDeleteRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(payload map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.sessions.delete.reply",
			Payload:    payload,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	if a.sessions == nil {
		return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
	}

	sid, _ := env.Payload["sessionId"].(string)
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return reply(map[string]any{"error": "SESSION_ID_REQUIRED"})
	}
	if !strings.HasPrefix(sid, "ls-") {
		return reply(map[string]any{"error": "NOT_LOGICAL", "detail": "only logical (ls-) ids are deletable via this endpoint"})
	}
	ls, ok := a.sessions.Get(logicalsession.ID(sid))
	if !ok {
		return reply(map[string]any{"error": "SESSION_NOT_FOUND"})
	}
	// Best-effort tear-down of the underlying CLI conversation.
	if ls.CLISessionID != "" {
		if drv, ok := a.router.Get(ls.Driver); ok {
			_ = drv.Stop(agent.SessionID(ls.CLISessionID))
		}
		if a.agentSessions != nil {
			a.agentSessions.drop(ls.CLISessionID)
		}
	}
	if err := a.sessions.Delete(ls.ID); err != nil {
		return reply(map[string]any{"error": "DELETE_SESSION_FAILED", "detail": err.Error()})
	}
	return reply(map[string]any{"ok": true})
}

// handleAgentMessagesRequest is the home-adapter side of the cloud reverse
// proxy for `GET /v1/agent/sessions/{id}/messages`. The cloud sends:
//
//	{type:"request", kind:"agent.messages",
//	 payload:{sessionId, driver, before, limit}}
//
// We dispatch to the local agent.MessageLoader (claudecode today) and reply
// with a single envelope:
//
//	{type:"reply", kind:"agent.messages.reply",
//	 payload:{messages, total, hasMore} | {error, detail}}
//
// Drivers without MessageLoader degrade to MESSAGES_NOT_SUPPORTED, mirroring
// the LAN-direct HTTP path's behaviour.
func (a *Adapter) handleAgentMessagesRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(payload map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.messages.reply",
			Payload:    payload,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	sid, _ := env.Payload["sessionId"].(string)
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return reply(map[string]any{"error": "SESSION_ID_REQUIRED"})
	}

	// ADR-014: resolve logical session ids (ls-*) to the underlying CLI
	// session id, same as the LAN-direct HTTP path in agent_messages.go.
	// Without this, LoadMessages receives an id the driver doesn't recognise
	// and returns an empty page.
	if a.sessions != nil && strings.HasPrefix(sid, "ls-") {
		ls, ok := a.sessions.Get(logicalsession.ID(sid))
		if !ok {
			return reply(map[string]any{"error": "SESSION_NOT_FOUND", "detail": "logical session not found: " + sid})
		}
		if ls.CLISessionID == "" {
			// Session exists but no CLI conversation started yet — empty page.
			return reply(map[string]any{"messages": []any{}, "total": 0, "hasMore": false})
		}
		sid = ls.CLISessionID
	}

	driverName, _ := env.Payload["driver"].(string)
	driverName = strings.TrimSpace(driverName)
	if driverName == "" {
		return reply(map[string]any{"error": "DRIVER_REQUIRED"})
	}
	drv, ok := a.router.Get(driverName)
	if !ok {
		return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": "driver not registered: " + driverName})
	}
	loader, ok := drv.(agent.MessageLoader)
	if !ok {
		return reply(map[string]any{"error": "MESSAGES_NOT_SUPPORTED",
			"detail": "driver " + driverName + " does not support message replay"})
	}

	before := -1
	if v, ok := env.Payload["before"].(float64); ok {
		before = int(v)
	}
	limit := 50
	if v, ok := env.Payload["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > 200 {
			limit = 200
		}
	}

	page, err := loader.LoadMessages(context.Background(), sid, before, limit)
	if err != nil {
		return reply(map[string]any{"error": "LOAD_MESSAGES_FAILED", "detail": err.Error()})
	}
	if page.Messages == nil {
		page.Messages = []agent.Message{}
	}
	return reply(map[string]any{
		"messages": page.Messages,
		"total":    page.Total,
		"hasMore":  page.HasMore,
	})
}

// handleAgentDriversRequest replies with the same shape as the cloud
// proxy expects (a flat `drivers` payload). The cloud HTTP layer reshapes
// that into the response.data.drivers envelope the firmware reads.
func (a *Adapter) handleAgentDriversRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	var drivers []map[string]any
	if a.router != nil {
		for _, info := range a.router.List() {
			drivers = append(drivers, map[string]any{
				"name":         info.Name,
				"capabilities": info.Capabilities,
			})
		}
	}
	if drivers == nil {
		drivers = []map[string]any{}
	}
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "agent.drivers.reply",
		Payload:    map[string]any{"drivers": drivers},
	})
}

// resolveCwdByName looks up a cwd path by name in the configured pool.
// Returns ("", false) when the name is not found.
func (a *Adapter) resolveCwdByName(name string) (string, bool) {
	for _, entry := range a.cwdPool {
		if entry.Name == name {
			return entry.Path, true
		}
	}
	return "", false
}

// handleAgentCwdPoolRequest replies with the configured CWD pool entries.
// The cloud HTTP layer reshapes that into the response.data.pool envelope
// the firmware reads via GET /v1/agent/cwd-pool.
func (a *Adapter) handleAgentCwdPoolRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	type poolItem struct {
		Name string `json:"name"`
	}
	var items []poolItem
	for _, e := range a.cwdPool {
		items = append(items, poolItem{Name: e.Name})
	}
	if items == nil {
		items = []poolItem{}
	}
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "agent.cwd_pool.reply",
		Payload:    map[string]any{"pool": items},
	})
}

// handleAgentMessageRequest runs one agent turn end-to-end and emits one
// `event` envelope per agent.Event, then a final `reply` envelope marking
// the turn complete (or failed). The cloud HTTP handler turns each `event`
// into one NDJSON line for the device.
//
// Frame mapping (cloud→device NDJSON `type`):
//
//	envelope.kind == "agent.event"     ->  payload["type"] is one of the agent
//	                                       NDJSON types ("session", "text",
//	                                       "tokens", "tool_call", "error")
//	envelope.kind == "agent.reply"     ->  the final reply: payload contains
//	                                       {"ok": true} on clean turn_end so the
//	                                       cloud knows to emit a `turn_end`
//	                                       NDJSON line. On failure, ok=false +
//	                                       error/detail.
//
// This split is intentional: streaming events are best-effort fire-and-forget
// (the WS relay drops them if the device dropped the HTTP connection), while
// the reply is the one frame the cloud waits on with a deadline.
func (a *Adapter) handleAgentMessageRequest(ctx context.Context, write func(CloudEnvelope) error, env CloudEnvelope) error {
	if a.router == nil {
		return errors.New("AGENT_NOT_CONFIGURED")
	}
	if a.agentSessions == nil {
		// Lazy init so test fixtures that build an Adapter literal also work.
		a.agentSessions = newAgentProxyRegistry()
	}

	text, _ := env.Payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("EMPTY_TEXT")
	}
	requestedDriver, _ := env.Payload["driver"].(string)
	requestedDriver = strings.TrimSpace(requestedDriver)
	requestedSession, _ := env.Payload["sessionId"].(string)
	requestedSession = strings.TrimSpace(requestedSession)

	// Pick a candidate driver. Mirrors httpapi.Server.handleAgentMessage so
	// behaviour matches between LAN-direct and proxied requests.
	var (
		drv        agent.Driver
		driverName string
	)
	if requestedDriver != "" {
		var ok bool
		drv, ok = a.router.Get(requestedDriver)
		if !ok {
			return fmt.Errorf("UNKNOWN_DRIVER:%s", requestedDriver)
		}
		driverName = requestedDriver
	} else {
		drv = a.router.Default()
		if drv == nil {
			return errors.New("AGENT_NOT_CONFIGURED")
		}
		driverName = drv.Name()
	}

	// Resolve or start the session. A reused session is pinned to whichever
	// driver started it; a contradicting driver request fails fast so the
	// caller learns about it rather than silently switching mid-conversation.
	var (
		sid   agent.SessionID
		isNew bool
	)

	writeEventEnv := func(payload map[string]any) error {
		return write(CloudEnvelope{
			Type:       "event",
			MessageID:  env.MessageID,
			DeviceID:   env.DeviceID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.event",
			Payload:    payload,
		})
	}

	// ADR-014 Phase B: when a logical-session manager is configured, resolve
	// "ls-"-prefixed sessions and auto-mint on empty. Legacy raw cli ids fall
	// through to the existing path unchanged for backward compatibility.
	var (
		logicalID         logicalsession.ID
		resumeFromLogical string
		usingLogical      bool
	)
	if a.sessions != nil {
		switch {
		case strings.HasPrefix(requestedSession, "ls-"):
			ls, ok := a.sessions.Get(logicalsession.ID(requestedSession))
			if !ok {
				// Don't silently mint a new logical id — emit an error event
				// so the cloud forwards it to the firmware as a normal turn
				// failure.
				if err := writeEventEnv(map[string]any{
					"type":  "error",
					"error": "UNKNOWN_LOGICAL_SESSION",
					"text":  "logical session not found: " + requestedSession,
				}); err != nil {
					a.log.Warnf("agent_proxy: write UNKNOWN_LOGICAL_SESSION failed device=%s err=%v",
						env.DeviceID, err)
				}
				return write(CloudEnvelope{
					Type:       "reply",
					MessageID:  env.MessageID,
					DeviceID:   env.DeviceID,
					HomeSiteID: a.cfg.HomeSiteID,
					Kind:       "agent.reply",
					Payload: map[string]any{
						"ok":    false,
						"error": "UNKNOWN_LOGICAL_SESSION",
					},
				})
			}
			logicalID = ls.ID
			resumeFromLogical = ls.CLISessionID
			usingLogical = true
			// If the logical's CLISessionID matches a live in-process entry,
			// honour the existing pinning behaviour.
			if resumeFromLogical != "" {
				if entry, found := a.agentSessions.get(resumeFromLogical); found {
					if requestedDriver != "" && requestedDriver != entry.driverName {
						return fmt.Errorf("SESSION_DRIVER_MISMATCH:want=%s,have=%s", requestedDriver, entry.driverName)
					}
					pinned, ok := a.router.Get(entry.driverName)
					if !ok {
						return fmt.Errorf("SESSION_UNREGISTERED_DRIVER:%s", entry.driverName)
					}
					drv = pinned
					driverName = entry.driverName
					sid = entry.sid
				}
			}
		case requestedSession == "":
			// Auto-mint a logical session so future turns can come back with
			// its stable id. Cwd defaults to the manager's configured value.
			ls, err := a.sessions.Create(env.DeviceID, driverName, "", "")
			if err != nil {
				return fmt.Errorf("CREATE_SESSION_FAILED:%w", err)
			}
			logicalID = ls.ID
			usingLogical = true
		}
	}

	if !usingLogical && requestedSession != "" {
		// Legacy raw CLI id path (Phase A backward compat).
		if entry, found := a.agentSessions.get(requestedSession); found {
			if requestedDriver != "" && requestedDriver != entry.driverName {
				return fmt.Errorf("SESSION_DRIVER_MISMATCH:want=%s,have=%s", requestedDriver, entry.driverName)
			}
			pinned, ok := a.router.Get(entry.driverName)
			if !ok {
				return fmt.Errorf("SESSION_UNREGISTERED_DRIVER:%s", entry.driverName)
			}
			drv = pinned
			driverName = entry.driverName
			sid = entry.sid
		}
	}

	writeEvent := func(payload map[string]any) {
		// Best-effort; if the WS write fails the cloud will time out the
		// pending reply, so swallow the error after a log.
		if err := write(CloudEnvelope{
			Type:       "event",
			MessageID:  env.MessageID,
			DeviceID:   env.DeviceID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.event",
			Payload:    payload,
		}); err != nil {
			a.log.Warnf("agent_proxy: write event failed device=%s err=%v", env.DeviceID, err)
		}
	}

	a.metrics.Inc("agent_proxy_message_start")
	routeStart := time.Now()

	// Outer loop wraps one or two attempts. Attempt 0 is the normal path
	// (start with ResumeID if the device asked to resume); attempt 1 fires
	// only when the driver emitted SESSION_NOT_FOUND on attempt 0 — see
	// ADR-014. We mint a fresh session and re-send the user text so the
	// device sees a clean turn instead of a generic AGENT_TURN_FAILED.
	const maxAttempts = 2
	var (
		turnEnded  bool
		textCount  int
		errorCount int
		lastError  string
		lastText   string
		sendErr    error
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Reset per-attempt state — only the FINAL attempt's tallies count
		// when we build the reply payload below.
		turnEnded = false
		textCount = 0
		errorCount = 0
		lastError = ""
		lastText = ""
		sendErr = nil
		sessionNotFound := false

		if sid == "" {
			// Resume path: if the device sent a sessionId we don't have in our
			// process registry (adapter restart / sweep / picker-loaded session),
			// pass it as ResumeID so claude-code continues the same JSONL. On
			// retry (attempt > 0) we deliberately DON'T resume — the prior id
			// just told us "no conversation found".
			startOpts := agent.StartOpts{}
			isResumeAttempt := false
			if attempt == 0 {
				switch {
				case usingLogical:
					if resumeFromLogical != "" {
						startOpts.ResumeID = resumeFromLogical
						isResumeAttempt = true
					}
				case requestedSession != "":
					startOpts.ResumeID = requestedSession
					isResumeAttempt = true
				}
			}
			newSid, err := drv.Start(ctx, startOpts)
			if err != nil {
				return fmt.Errorf("AGENT_START_FAILED:%w", err)
			}
			sid = newSid
			isNew = !isResumeAttempt
			a.agentSessions.put(string(sid), &agentProxySession{
				sid:        sid,
				driverName: driverName,
				lastUsed:   time.Now(),
			})
			// Write back the freshly-minted CLI session id to the logical
			// table on every Start (including retry) so the logical's
			// CLISessionID always tracks the live conversation.
			if usingLogical && logicalID != "" {
				if err := a.sessions.UpdateCLISessionID(logicalID, string(sid)); err != nil {
					a.log.Warnf("agent_proxy: UpdateCLISessionID logical=%s cli=%s err=%v",
						logicalID, sid, err)
				}
			}
		}

		// Session frame first so the device learns its session id before any
		// text arrives. seq=0 mirrors the LAN direct shape. On retry the
		// device sees a second session frame with the new sid+isNew=true,
		// which is exactly what its NVS needs to update.
		//
		// When the logical-session manager is in play, the device-visible id
		// in the frame is the *logical* id (stable across cli rotation).
		visibleSessionID := string(sid)
		if usingLogical && logicalID != "" {
			visibleSessionID = string(logicalID)
		}
		writeEvent(map[string]any{
			"type":      "session",
			"sessionId": visibleSessionID,
			"isNew":     isNew,
			"driver":    driverName,
			"seq":       0,
		})
		a.agentSessions.touch(string(sid))

		if attempt == 0 {
			a.log.Infof("phase=agent_proxy_start driver=%s sid=%s is_new=%v device=%s text_chars=%d",
				driverName, sid, isNew, env.DeviceID, len(text))
		} else {
			a.log.Warnf("phase=agent_proxy_retry driver=%s sid=%s device=%s attempt=%d reason=SESSION_NOT_FOUND",
				driverName, sid, env.DeviceID, attempt)
		}

		events := drv.Events(sid)
		sendErrCh := make(chan error, 1)
		curSid := sid
		go func() { sendErrCh <- drv.Send(curSid, text) }()

	loop:
		for {
			select {
			case <-ctx.Done():
				a.log.Warnf("agent_proxy: ctx done driver=%s sid=%s", driverName, sid)
				return ctx.Err()
			case ev, ok := <-events:
				if !ok {
					// Driver closed channel — session ended on its side. Drop
					// the registry entry so the next turn doesn't try to resume
					// a dead session.
					a.agentSessions.drop(string(sid))
					break loop
				}
				switch ev.Type {
				case agent.EvSessionInit:
					// CLI reported its real session id. Update the logical
					// session store so LoadMessages can find the JSONL file.
					if usingLogical && logicalID != "" && ev.Text != "" {
						if err := a.sessions.UpdateCLISessionID(logicalID, ev.Text); err != nil {
							a.log.Warnf("agent_proxy: UpdateCLISessionID (init) logical=%s cli=%s err=%v",
								logicalID, ev.Text, err)
						}
					}
				case agent.EvText:
					textCount++
					lastText = ev.Text
				case agent.EvError:
					errorCount++
					lastError = ev.Text
					if strings.HasPrefix(ev.Text, "SESSION_NOT_FOUND") {
						sessionNotFound = true
					}
				case agent.EvTurnEnd:
					turnEnded = true
					break loop
				}
				if ev.Type == agent.EvTurnEnd {
					continue
				}
				// Suppress the SESSION_NOT_FOUND error frame on the attempt
				// we plan to retry — the device should not see a transient
				// error message that we're about to recover from.
				if ev.Type == agent.EvError && sessionNotFound && attempt+1 < maxAttempts {
					continue
				}
				frame := agentEventToFrame(ev)
				if frame != nil {
					writeEvent(frame)
				}
			}
		}

		sendErr = <-sendErrCh
		if sendErr != nil {
			a.log.Warnf("phase=agent_proxy_send_failed driver=%s sid=%s err=%v attempt=%d",
				driverName, sid, sendErr, attempt)
		}

		if sessionNotFound && attempt+1 < maxAttempts {
			// Drop the dead session and force a fresh start on next iteration.
			a.agentSessions.drop(string(sid))
			sid = ""
			isNew = false // will be flipped to true by the no-resume branch
			a.metrics.Inc("agent_proxy_session_not_found_retry")
			continue
		}
		break
	}

	a.agentSessions.touch(string(sid))

	// Bump LastUsedAt on the logical session so the picker can sort by recency.
	if usingLogical && logicalID != "" && turnEnded {
		if err := a.sessions.Touch(logicalID); err != nil {
			a.log.Warnf("agent_proxy: Touch logical=%s err=%v", logicalID, err)
		}
	}

	// Phase S3: push session.notification via cloud WS — emitted once per
	// turn after retry has settled, so the device gets a single notification
	// reflecting the actual outcome rather than an intermediate failure.
	if turnEnded {
		notifType := "turn_end"
		if errorCount > 0 && textCount == 0 {
			notifType = "error"
		}
		preview := lastText
		if len(preview) > 48 {
			preview = preview[:48]
		}
		notifSID := string(sid)
		if usingLogical && logicalID != "" {
			notifSID = string(logicalID)
		}
		_ = write(CloudEnvelope{
			Type:       "event",
			DeviceID:   env.DeviceID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "session.notification",
			Payload: map[string]any{
				"sessionId": notifSID,
				"driver":    driverName,
				"type":      notifType,
				"preview":   preview,
				"timestamp": time.Now().UnixMilli(),
			},
		})
	}

	// Turn is healthy only when it ended cleanly AND either produced text or
	// had no errors. A turn with only errors and zero text is a silent failure
	// from the user's perspective — report it so the cloud emits an error
	// frame the firmware can display.
	turnOK := turnEnded && (errorCount == 0 || textCount > 0)

	a.log.Infof("phase=agent_proxy_done driver=%s sid=%s elapsed_s=%.3f turn_end=%v ok=%v text=%d errors=%d",
		driverName, sid, time.Since(routeStart).Seconds(), turnEnded, turnOK, textCount, errorCount)
	if turnOK {
		a.metrics.Inc("agent_proxy_message_ok")
	} else {
		a.metrics.Inc("agent_proxy_message_error")
	}

	replySID := string(sid)
	if usingLogical && logicalID != "" {
		replySID = string(logicalID)
	}
	replyPayload := map[string]any{
		"ok":        turnOK,
		"sessionId": replySID,
		"driver":    driverName,
		"turnEnd":   turnEnded,
	}
	if !turnOK {
		errMsg := "AGENT_TURN_FAILED"
		if sendErr != nil {
			errMsg = sendErr.Error()
		}
		replyPayload["error"] = errMsg
		if lastError != "" {
			replyPayload["detail"] = lastError
		}
	}

	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		DeviceID:   env.DeviceID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "agent.reply",
		Payload:    replyPayload,
	})
}

// agentEventToFrame mirrors httpapi.Server.writeAgentEvent. Returns the
// payload that goes inside a `Kind: "agent.event"` envelope. Returning nil
// means "skip this event" (currently we forward every event type).
func agentEventToFrame(ev agent.Event) map[string]any {
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
		return nil
	}
	return frame
}

// runAgentSessionSweeper periodically drops sessions that haven't been
// touched for ttl. Started by Adapter.Run if the router is wired.
func (a *Adapter) runAgentSessionSweeper(ctx context.Context, ttl time.Duration) {
	t := time.NewTicker(ttl / 6) // sweep ~5min for 30min ttl, matches httpapi.
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweepAgentSessions(ttl)
		}
	}
}

func (a *Adapter) sweepAgentSessions(ttl time.Duration) {
	if a.agentSessions == nil || a.router == nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	a.agentSessions.mu.Lock()
	stale := make([]*agentProxySession, 0)
	for id, e := range a.agentSessions.sessions {
		if e.lastUsed.Before(cutoff) {
			stale = append(stale, e)
			delete(a.agentSessions.sessions, id)
		}
	}
	a.agentSessions.mu.Unlock()
	for _, e := range stale {
		drv, ok := a.router.Get(e.driverName)
		if !ok {
			a.log.Warnf("agent_proxy: sweep found stale entry with unknown driver=%s sid=%s", e.driverName, e.sid)
			continue
		}
		if err := drv.Stop(e.sid); err != nil {
			a.log.Warnf("agent_proxy: sweep stop driver=%s sid=%s err=%v", e.driverName, e.sid, err)
			continue
		}
		a.log.Infof("agent_proxy: swept idle driver=%s sid=%s", e.driverName, e.sid)
	}
}
