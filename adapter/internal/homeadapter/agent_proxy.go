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
	if requestedSession != "" {
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

	if sid == "" {
		newSid, err := drv.Start(ctx, agent.StartOpts{})
		if err != nil {
			return fmt.Errorf("AGENT_START_FAILED:%w", err)
		}
		sid = newSid
		isNew = true
		a.agentSessions.put(string(sid), &agentProxySession{
			sid:        sid,
			driverName: driverName,
			lastUsed:   time.Now(),
		})
	}

	// Session frame first so the device learns its session id before any
	// text arrives. seq=0 mirrors the LAN direct shape.
	writeEvent(map[string]any{
		"type":      "session",
		"sessionId": string(sid),
		"isNew":     isNew,
		"driver":    driverName,
		"seq":       0,
	})
	a.agentSessions.touch(string(sid))

	events := drv.Events(sid)
	sendErrCh := make(chan error, 1)
	go func() { sendErrCh <- drv.Send(sid, text) }()

	a.metrics.Inc("agent_proxy_message_start")
	routeStart := time.Now()
	a.log.Infof("phase=agent_proxy_start driver=%s sid=%s is_new=%v device=%s text_chars=%d",
		driverName, sid, isNew, env.DeviceID, len(text))

	var (
		turnEnded  bool
		textCount  int
		errorCount int
		lastError  string
		lastText   string
	)
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
			case agent.EvText:
				textCount++
				lastText = ev.Text
			case agent.EvError:
				errorCount++
				lastError = ev.Text
			case agent.EvTurnEnd:
				turnEnded = true
				// Phase S3: push session.notification via cloud WS.
				notifType := "turn_end"
				if errorCount > 0 && textCount == 0 {
					notifType = "error"
				}
				preview := lastText
				if len(preview) > 48 {
					preview = preview[:48]
				}
				_ = write(CloudEnvelope{
					Type:       "event",
					DeviceID:   env.DeviceID,
					HomeSiteID: a.cfg.HomeSiteID,
					Kind:       "session.notification",
					Payload: map[string]any{
						"sessionId": string(sid),
						"driver":    driverName,
						"type":      notifType,
						"preview":   preview,
						"timestamp": time.Now().UnixMilli(),
					},
				})
				break loop
			}
			if ev.Type != agent.EvTurnEnd {
				frame := agentEventToFrame(ev)
				if frame != nil {
					writeEvent(frame)
				}
			}
		}
	}

	sendErr := <-sendErrCh
	if sendErr != nil {
		a.log.Warnf("phase=agent_proxy_send_failed driver=%s sid=%s err=%v", driverName, sid, sendErr)
	}
	a.agentSessions.touch(string(sid))

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

	replyPayload := map[string]any{
		"ok":        turnOK,
		"sessionId": string(sid),
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
