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
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/menu"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/detect"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
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
	sessions := a.sessions.ListDeviceFacing(deviceID, driverName, limit)
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
//
// Each driver row carries `models` (when the driver implements
// agent.ModelLister) and `active_model` (from driverState). The top-level
// payload also includes `active_driver`. These fields match the LAN-direct
// HTTP response in httpapi.handleAgentDrivers so the firmware code path is
// identical across cloud_saas and local_home.
func (a *Adapter) handleAgentDriversRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	drivers := make([]map[string]any, 0)
	activeDriver := ""
	if a.router != nil {
		if a.driverState != nil {
			if name := a.driverState.ActiveDriver(); name != "" {
				if _, ok := a.router.Get(name); ok {
					activeDriver = name
				}
			}
		}
		if activeDriver == "" {
			if d := a.router.Default(); d != nil {
				activeDriver = d.Name()
			}
		}
		installed := detect.InstalledByDriver()
		for _, info := range a.router.List() {
			row := map[string]any{
				"name":           info.Name,
				"capabilities":   info.Capabilities,
				"butler_capable": info.Capabilities.Butler,
			}
			if present, ok := installed[info.Name]; ok {
				row["installed"] = present
			}
			if drv, ok := a.router.Get(info.Name); ok {
				if ml, isLister := drv.(agent.ModelLister); isLister {
					if models, err := ml.ListModels(context.Background()); err == nil {
						row["models"] = models
					} else {
						a.log.Warnf("agent_proxy: driver %s ListModels failed: %v", info.Name, err)
					}
				}
			}
			if m := a.resolveActiveModel(info.Name); m != "" {
				row["active_model"] = m
			}
			drivers = append(drivers, row)
		}
	}
	payload := map[string]any{"drivers": drivers}
	if activeDriver != "" {
		payload["active_driver"] = activeDriver
	}
	payload["butler_driver"] = a.resolveButlerDriver()
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "agent.drivers.reply",
		Payload:    payload,
	})
}

// handleAgentActiveDriverSetRequest persists a new active_driver selection
// from the cloud. Mirrors PUT /v1/agent/active_driver.
//
//	{type:"request", kind:"agent.active_driver.set",
//	 payload:{"name":"opencode"}}
//
// Reply kind "agent.active_driver.set.reply" with {ok:true,active_driver}
// on success or {error,detail} on failure.
func (a *Adapter) handleAgentActiveDriverSetRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(p map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.active_driver.set.reply",
			Payload:    p,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	if a.driverState == nil {
		return reply(map[string]any{"error": "DRIVERSTATE_NOT_CONFIGURED"})
	}
	name, _ := env.Payload["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return reply(map[string]any{"error": "EMPTY_NAME"})
	}
	if _, ok := a.router.Get(name); !ok {
		return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": name})
	}
	if err := a.driverState.SetActiveDriver(name); err != nil {
		return reply(map[string]any{"error": "PERSIST_FAILED", "detail": err.Error()})
	}
	a.router.SetDefault(name)
	a.log.Infof("agent_proxy: active_driver set to %q", name)
	return reply(map[string]any{"ok": true, "active_driver": name})
}


// resolveButlerDriver mirrors httpapi.Server.resolveButlerDriver for the cloud
// path (ADR-024 §1): the single active_driver backs the butler when it is
// registered and butler-capable, else the claude-code fallback.
func (a *Adapter) resolveButlerDriver() string {
	if name := a.resolveActiveDriver(); name != "" {
		if drv, ok := a.router.Get(name); ok && drv.Capabilities().Butler {
			return name
		}
		a.log.Warnf("agent_proxy: active_driver=%q not butler-capable, butler falls back to %q", name, butler.ButlerDriver)
	}
	return butler.ButlerDriver
}

// handleAgentActiveModelSetRequest persists an active model for one driver.
// Mirrors PUT /v1/agent/drivers/{name}/active_model.
//
//	{type:"request", kind:"agent.active_model.set",
//	 payload:{"driver":"claude-code","model":"claude-opus-4-7"}}
//
// model="" clears the override. Reply kind "agent.active_model.set.reply".
func (a *Adapter) handleAgentActiveModelSetRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(p map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.active_model.set.reply",
			Payload:    p,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	if a.driverState == nil {
		return reply(map[string]any{"error": "DRIVERSTATE_NOT_CONFIGURED"})
	}
	driver, _ := env.Payload["driver"].(string)
	driver = strings.TrimSpace(driver)
	if driver == "" {
		return reply(map[string]any{"error": "EMPTY_DRIVER"})
	}
	if _, ok := a.router.Get(driver); !ok {
		return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": driver})
	}
	model, _ := env.Payload["model"].(string)
	model = strings.TrimSpace(model)
	if err := a.driverState.SetActiveModel(driver, model); err != nil {
		return reply(map[string]any{"error": "PERSIST_FAILED", "detail": err.Error()})
	}
	a.log.Infof("agent_proxy: %s active_model set to %q", driver, model)
	return reply(map[string]any{"ok": true, "driver": driver, "active_model": model})
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

// resolveActiveDriver mirrors httpapi.Server.resolveActiveDriver: persisted
// active driver if registered, else the router default.
func (a *Adapter) resolveActiveDriver() string {
	if a.driverState != nil {
		if name := a.driverState.ActiveDriver(); name != "" {
			if _, ok := a.router.Get(name); ok {
				return name
			}
		}
	}
	if d := a.router.Default(); d != nil {
		return d.Name()
	}
	return ""
}

// cwdDisplayName mirrors httpapi.Server.cwdDisplayName (path→pool name, basename
// fallback, empty for empty cwd).
func (a *Adapter) cwdDisplayName(cwd string) string {
	if cwd == "" {
		return ""
	}
	for _, e := range a.cwdPool {
		if e.Path == cwd {
			return e.Name
		}
	}
	return filepath.Base(cwd)
}

// menuToPayload serialises a menu.Menu / menu.Result into the map[string]any a
// CloudEnvelope carries, so the cloud HTTP layer passes it straight through as
// the device's response `data` (byte-identical to the LAN-direct path).
func menuToPayload(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// handleAgentMenuRequest mirrors httpapi.handleAgentMenu for cloud_saas (ADR-019).
// Menu shaping is shared via internal/agent/menu; only data resolution + the
// envelope reply differ from the LAN-direct handler.
//
//	{type:"request", kind:"agent.menu", payload:{"id":..,"deviceId":..,"driver":..,"current":..}}
func (a *Adapter) handleAgentMenuRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(p map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.menu.reply",
			Payload:    p,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	var p struct {
		ID       string `json:"id"`
		DeviceID string `json:"deviceId"`
		Driver   string `json:"driver"`
		Current  string `json:"current"`
	}
	raw, _ := json.Marshal(env.Payload)
	_ = json.Unmarshal(raw, &p)

	switch strings.TrimSpace(p.ID) {
	case "drivers":
		return reply(menuToPayload(menu.Drivers(a.router.List(), a.resolveActiveDriver())))
	case "models":
		driver := strings.TrimSpace(p.Driver)
		if driver == "" {
			driver = a.resolveActiveDriver()
		}
		drv, ok := a.router.Get(driver)
		if !ok {
			return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": driver})
		}
		var models []agent.ModelInfo
		if ml, isLister := drv.(agent.ModelLister); isLister {
			if mm, err := ml.ListModels(context.Background()); err == nil {
				models = mm
			} else {
				a.log.Warnf("agent_proxy menu/models: driver %s ListModels failed: %v", driver, err)
			}
		}
		return reply(menuToPayload(menu.Models(driver, models, a.resolveActiveModel(driver))))
	case "sessions":
		if a.sessions == nil {
			return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
		}
		driver := strings.TrimSpace(p.Driver)
		if driver == "" {
			driver = a.resolveActiveDriver()
		}
		now := time.Now()
		items := make([]menu.SessionItem, 0)
		// NB: cloud path doesn't filter by SessionMaxAge (homeadapter Config has
		// none; mirrors handleAgentSessionsListLogicalRequest which lists all).
		for _, sess := range a.sessions.ListDeviceFacing(strings.TrimSpace(p.DeviceID), driver, 50) {
			items = append(items, menu.SessionItem{
				ID:         string(sess.ID),
				Title:      sess.Title,
				Driver:     sess.Driver,
				CwdName:    a.cwdDisplayName(sess.Cwd),
				LastUsedAt: sess.LastUsedAt,
			})
		}
		return reply(menuToPayload(menu.Sessions(items, strings.TrimSpace(p.Current), now)))
	case "cwd":
		names := make([]string, 0, len(a.cwdPool))
		for _, e := range a.cwdPool {
			names = append(names, e.Name)
		}
		return reply(menuToPayload(menu.Cwd(names)))
	default:
		return reply(map[string]any{"error": "UNKNOWN_MENU", "detail": strings.TrimSpace(p.ID)})
	}
}

// handleAgentMenuActionRequest mirrors httpapi.handleAgentMenuAction (ADR-019).
//
//	{type:"request", kind:"agent.menu.action", payload:{"deviceId":..,"action":{...}}}
func (a *Adapter) handleAgentMenuActionRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
	reply := func(p map[string]any) error {
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "agent.menu.action.reply",
			Payload:    p,
		})
	}
	if a.router == nil {
		return reply(map[string]any{"error": "AGENT_NOT_CONFIGURED"})
	}
	var body struct {
		DeviceID string      `json:"deviceId"`
		Action   menu.Action `json:"action"`
	}
	raw, _ := json.Marshal(env.Payload)
	_ = json.Unmarshal(raw, &body)
	act := body.Action

	switch act.Type {
	case "set_driver":
		if a.driverState == nil {
			return reply(map[string]any{"error": "DRIVERSTATE_NOT_CONFIGURED"})
		}
		name := strings.TrimSpace(act.Driver)
		if name == "" {
			return reply(map[string]any{"error": "EMPTY_NAME"})
		}
		if _, ok := a.router.Get(name); !ok {
			return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": name})
		}
		if err := a.driverState.SetActiveDriver(name); err != nil {
			return reply(map[string]any{"error": "PERSIST_FAILED", "detail": err.Error()})
		}
		a.router.SetDefault(name)
		a.log.Infof("agent_proxy menu/action: set_driver=%q", name)
		return reply(menuToPayload(menu.Result{Result: "closed"}))
	case "set_model":
		if a.driverState == nil {
			return reply(map[string]any{"error": "DRIVERSTATE_NOT_CONFIGURED"})
		}
		name := strings.TrimSpace(act.Driver)
		if name == "" {
			return reply(map[string]any{"error": "EMPTY_NAME"})
		}
		if _, ok := a.router.Get(name); !ok {
			return reply(map[string]any{"error": "UNKNOWN_DRIVER", "detail": name})
		}
		if err := a.driverState.SetActiveModel(name, strings.TrimSpace(act.Model)); err != nil {
			return reply(map[string]any{"error": "PERSIST_FAILED", "detail": err.Error()})
		}
		a.log.Infof("agent_proxy menu/action: set_model driver=%q model=%q", name, strings.TrimSpace(act.Model))
		return reply(menuToPayload(menu.Result{Result: "closed"}))
	case "select_session":
		if a.sessions == nil {
			return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
		}
		id := strings.TrimSpace(act.SessionID)
		if id == "" {
			return reply(map[string]any{"error": "EMPTY_SESSION_ID"})
		}
		if _, ok := a.sessions.Get(logicalsession.ID(id)); !ok {
			return reply(map[string]any{"error": "UNKNOWN_LOGICAL_SESSION", "detail": id})
		}
		return reply(menuToPayload(menu.Result{Result: "closed", SessionID: id, LoadHistory: true}))
	case "create_session":
		if a.sessions == nil {
			return reply(map[string]any{"error": "LOGICAL_SESSIONS_DISABLED"})
		}
		driver := a.resolveActiveDriver()
		if driver == "" {
			return reply(map[string]any{"error": "DRIVER_REQUIRED"})
		}
		cwd := ""
		if cwdName := strings.TrimSpace(act.Cwd); cwdName != "" {
			resolved, ok := a.resolveCwdByName(cwdName)
			if !ok {
				return reply(map[string]any{"error": "UNKNOWN_CWD_NAME", "detail": cwdName})
			}
			cwd = resolved
		}
		sess, err := a.sessions.Create(strings.TrimSpace(body.DeviceID), driver, cwd, "")
		if err != nil {
			return reply(map[string]any{"error": "CREATE_SESSION_FAILED", "detail": err.Error()})
		}
		a.log.Infof("agent_proxy menu/action: create_session logical=%s driver=%s cwd=%q", sess.ID, driver, cwd)
		return reply(menuToPayload(menu.Result{Result: "closed", SessionID: string(sess.ID), LoadHistory: true}))
	case "open_menu":
		switch strings.TrimSpace(act.MenuID) {
		case "cwd":
			names := make([]string, 0, len(a.cwdPool))
			for _, e := range a.cwdPool {
				names = append(names, e.Name)
			}
			m := menu.Cwd(names)
			return reply(menuToPayload(menu.Result{Result: "navigate", NextMenu: &m}))
		default:
			return reply(map[string]any{"error": "UNSUPPORTED_MENU", "detail": strings.TrimSpace(act.MenuID)})
		}
	case "close":
		return reply(menuToPayload(menu.Result{Result: "closed"}))
	default:
		return reply(map[string]any{"error": "UNSUPPORTED_ACTION", "detail": act.Type})
	}
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

	routeStart := time.Now()

	sink := &cloudEventSink{a: a, write: write, env: env}

	// Heartbeat: initialise shared activity tracking and start background ticker.
	var lastActivity atomic.Int64
	var currentPhase atomic.Value
	currentPhase.Store("thinking")
	lastActivity.Store(time.Now().UnixNano())
	sink.lastActivity = &lastActivity
	sink.currentPhase = &currentPhase
	stopHeartbeat := a.startHeartbeat(ctx, write, env, &lastActivity, &currentPhase)
	defer stopHeartbeat()

	eng := butler.NewEngine(butler.Deps{
		Router:   a.router,
		Sessions: a.sessions,
		Registry: &cloudSessionRegistry{r: a.agentSessions},
		Sink:     sink,
		Hooks: butler.Hooks{
			OnStateChange: nil,
			OnTurnComplete: func(n butler.Notification) {
				preview := n.Preview
				if len(preview) > 48 {
					preview = preview[:48]
				}
				_ = write(CloudEnvelope{
					Type:       "event",
					DeviceID:   env.DeviceID,
					HomeSiteID: a.cfg.HomeSiteID,
					Kind:       "session.notification",
					Payload: map[string]any{
						"sessionId": n.SessionID,
						"driver":    n.Driver,
						"type":      n.Type,
						"preview":   preview,
						"timestamp": time.Now().UnixMilli(),
					},
				})
			},
			OnFinalReply: func(res *butler.Result) {
				// Turn is healthy only when it ended cleanly AND either produced
				// text or had no errors. A turn with only errors and zero text is
				// a silent failure — report it so the cloud emits an error frame.
				turnOK := res.TurnEnded && (res.ErrorCount == 0 || res.TextCount > 0)
				a.log.Infof("phase=agent_proxy_done driver=%s sid=%s elapsed_s=%.3f turn_end=%v ok=%v text=%d errors=%d",
					res.DriverName, res.FinalSID, time.Since(routeStart).Seconds(), res.TurnEnded, turnOK, res.TextCount, res.ErrorCount)
				// 收尾计数(agent_proxy_message_ok/error)统一由 butler 经
				// cloudMetrics.TurnDone 发出,此处不再重复 Inc,避免双计数。
				replyPayload := map[string]any{
					"ok":        turnOK,
					"sessionId": res.VisibleID,
					"driver":    res.DriverName,
					"turnEnd":   res.TurnEnded,
				}
				if !turnOK {
					errMsg := "AGENT_TURN_FAILED"
					if res.SendErr != nil {
						errMsg = res.SendErr.Error()
					}
					replyPayload["error"] = errMsg
					if res.LastError != "" {
						replyPayload["detail"] = res.LastError
					}
				}
				_ = write(CloudEnvelope{
					Type:       "reply",
					MessageID:  env.MessageID,
					DeviceID:   env.DeviceID,
					HomeSiteID: a.cfg.HomeSiteID,
					Kind:       "agent.reply",
					Payload:    replyPayload,
				})
			},
		},
		Policy: butler.Policy{
			ReuseWindow:          0,
			AllowBareCLIID:       true,
			AutoTitle:            false,
			EmitTurnEndFrame:     false,
			EmitStartFailedFrame: false,
			MaxAttempts:          2,
		},
		Metrics:            &cloudMetrics{m: a.metrics},
		Log:                a.log,
		Inflight:           a.inflight,
		ResolveActiveModel: a.resolveActiveModel,
		SystemPrompt:       butler.DeviceSystemPrompt,
		// Shared with the local path so cloud-relayed butler turns persist
		// long-term memory + show up in dispatch history (ADR-021 §4).
		Memory:           a.memory,
		DispatchRing:     a.dispatchRing,
		DispatchRecorder: a.dispatchRecorder,
		StartCtx:         ctx,
	})

	_, runErr := eng.RunTurn(ctx, butler.Request{
		Text:             text,
		RequestedDriver:  requestedDriver,
		RequestedSession: requestedSession,
		DeviceID:         env.DeviceID,
	})
	if runErr == nil {
		return nil
	}

	// UNKNOWN_LOGICAL_SESSION already emitted its error event via the Sink;
	// surface the final agent.reply{ok:false} the cloud expects, then swallow
	// the CodedError (it's not a transport-level failure).
	var ce *butler.CodedError
	if errors.As(runErr, &ce) {
		if ce.Code == "UNKNOWN_LOGICAL_SESSION" {
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
		// Other CodedErrors translate back to the historical Go-error strings
		// the cloud HTTP layer maps to firmware responses.
		return cloudErrorFromCoded(ce)
	}
	// ctx cancellation or other non-coded error (e.g. ctx.Err()).
	return runErr
}

// cloudErrorFromCoded translates a butler CodedError back into the exact
// Go-error string the cloud proxy historically returned, so the cloud HTTP
// layer's mapping to firmware responses is unchanged.
func cloudErrorFromCoded(ce *butler.CodedError) error {
	switch ce.Code {
	case "UNKNOWN_DRIVER":
		// Detail = "driver not registered: <name>"; historical form is the bare
		// driver name after the colon.
		name := strings.TrimPrefix(ce.Detail, "driver not registered: ")
		return fmt.Errorf("UNKNOWN_DRIVER:%s", name)
	case "SESSION_DRIVER_MISMATCH":
		// Detail already carries "want=..,have=..".
		return fmt.Errorf("SESSION_DRIVER_MISMATCH:%s", ce.Detail)
	case "SESSION_UNREGISTERED_DRIVER":
		return fmt.Errorf("SESSION_UNREGISTERED_DRIVER:%s", ce.Detail)
	case "CREATE_SESSION_FAILED":
		if ce.Err != nil {
			return fmt.Errorf("CREATE_SESSION_FAILED:%w", ce.Err)
		}
		return fmt.Errorf("CREATE_SESSION_FAILED:%s", ce.Detail)
	case "AGENT_START_FAILED":
		if ce.Err != nil {
			return fmt.Errorf("AGENT_START_FAILED:%w", ce.Err)
		}
		return fmt.Errorf("AGENT_START_FAILED:%s", ce.Detail)
	default:
		return errors.New(ce.Code)
	}
}

// cloudEventSink adapts the CloudEnvelope writer to butler.EventSink. All
// methods are best-effort (CLOUD streaming never aborts on write failure), so
// they return true unconditionally — matching the original writeEvent
// behaviour where a dropped WS write is logged but never short-circuits the
// turn.
type cloudEventSink struct {
	a     *Adapter
	write func(CloudEnvelope) error
	env   CloudEnvelope

	// lastActivity and currentPhase are shared with the heartbeat goroutine
	// started in handleAgentMessageRequest. Atomic so the goroutine reads are safe.
	lastActivity *atomic.Int64
	currentPhase *atomic.Value
}

func (c *cloudEventSink) emit(payload map[string]any) {
	if err := c.write(CloudEnvelope{
		Type:       "event",
		MessageID:  c.env.MessageID,
		DeviceID:   c.env.DeviceID,
		HomeSiteID: c.a.cfg.HomeSiteID,
		Kind:       "agent.event",
		Payload:    payload,
	}); err != nil {
		c.a.log.Warnf("agent_proxy: write event failed device=%s err=%v", c.env.DeviceID, err)
	}
}

func (c *cloudEventSink) EmitSession(visibleID string, isNew bool, driver string) bool {
	c.emit(map[string]any{
		"type":      "session",
		"sessionId": visibleID,
		"isNew":     isNew,
		"driver":    driver,
		"seq":       0,
	})
	return true
}

func (c *cloudEventSink) EmitEvent(ev agent.Event) bool {
	if frame := agentEventToFrame(ev); frame != nil {
		// Update activity tracking so the heartbeat goroutine knows the turn is
		// still making progress. EvText and EvToolCall are the meaningful
		// milestones; other event types (tokens, etc.) also count as activity.
		if c.lastActivity != nil {
			c.lastActivity.Store(time.Now().UnixNano())
			switch ev.Type {
			case agent.EvText:
				c.currentPhase.Store("generating")
			case agent.EvToolCall:
				c.currentPhase.Store("tool_call")
			}
		}
		c.emit(frame)
	}
	return true
}

func (c *cloudEventSink) EmitError(code, text string, detailField bool) bool {
	frame := map[string]any{"type": "error", "error": code}
	if detailField {
		frame["detail"] = text
	} else {
		frame["text"] = text
	}
	c.emit(frame)
	return true
}

// cloudSessionRegistry adapts *agentProxyRegistry to butler.SessionRegistry.
// SetState is a no-op because agentProxySession has no state field (差异 #11).
type cloudSessionRegistry struct{ r *agentProxyRegistry }

func (a *cloudSessionRegistry) Get(id string) (string, agent.SessionID, bool) {
	e, ok := a.r.get(id)
	if !ok {
		return "", "", false
	}
	return e.driverName, e.sid, true
}

func (a *cloudSessionRegistry) Put(id string, driverName string, sid agent.SessionID) {
	a.r.put(id, &agentProxySession{sid: sid, driverName: driverName, lastUsed: time.Now()})
}

func (a *cloudSessionRegistry) Touch(id string)           { a.r.touch(id) }
func (a *cloudSessionRegistry) Drop(id string)            { a.r.drop(id) }
func (a *cloudSessionRegistry) SetState(id, state string) {}

// cloudMetrics maps butler 的语义化指标事件到 cloud-proxy 的历史计数器名,逐字复刻
// 原 handleAgentMessage(cloud)的指标:start/ok/error 带 _message_ 中缀,
// retry/resume_skipped 不带;收尾 ok/error 的判定为 turnEnded&&(errorCount==0||textCount>0)。
type cloudMetrics struct{ m *obs.Metrics }

func (c *cloudMetrics) TurnStart()            { c.m.Inc("agent_proxy_message_start") }
func (c *cloudMetrics) ResumeSkippedMissing() { c.m.Inc("agent_proxy_resume_skipped_missing") }
func (c *cloudMetrics) SessionNotFoundRetry() { c.m.Inc("agent_proxy_session_not_found_retry") }
func (c *cloudMetrics) TurnDone(turnEnded bool, textCount, errorCount int) {
	turnOK := turnEnded && (errorCount == 0 || textCount > 0)
	if turnOK {
		c.m.Inc("agent_proxy_message_ok")
	} else {
		c.m.Inc("agent_proxy_message_error")
	}
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
