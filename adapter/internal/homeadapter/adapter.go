package homeadapter

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/openclaw"
	"github.com/gorilla/websocket"
)

type CloudEnvelope struct {
	Type       string         `json:"type"`
	MessageID  string         `json:"messageId,omitempty"`
	DeviceID   string         `json:"deviceId,omitempty"`
	HomeSiteID string         `json:"homeSiteId,omitempty"`
	SessionID  string         `json:"sessionId,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type Adapter struct {
	cfg     Config
	sink    transcriptSink
	router  *agent.Router
	log     *obs.Logger
	metrics *obs.Metrics
	dialer  *websocket.Dialer

	mu          sync.Mutex
	lastRegCode string
	status      Status

	// agentSessions holds long-lived agent sessions reused across cloud-
	// proxied turns (Phase 4.8). Lazily initialised the first time an
	// agent.message request arrives, so existing test fixtures that build
	// an Adapter literal keep working without changes.
	agentSessions *agentProxyRegistry

	// sessions is the logical-session table (ADR-014). Optional: when nil the
	// adapter falls back to legacy behaviour (raw CLI session ids on the
	// wire). Set via SetSessionManager from main.go.
	sessions *logicalsession.Manager

	// cwdPool holds the configured CWD pool entries (issue #30). Set via
	// SetCwdPool from main.go so cloud-proxied GET /v1/agent/cwd-pool works.
	cwdPool []CwdPoolEntry

	// driverState persists user-mutable driver preferences. Optional: when
	// nil the agent proxy uses router defaults. Set via SetDriverState.
	driverState *driverstate.Store

	// butlerWorkspace is the per-device butler workspace cwd (ADR-021 §1). When
	// non-empty (wired by main.go), the voice transcript path routes every turn
	// to the device's butler logical session via the butler engine instead of
	// the legacy one-shot driver spawn. Empty keeps the legacy voice path.
	butlerWorkspace string
	// butlerMCPConfig is the path to the butler's --mcp-config file (ADR-021
	// §2), passed to the engine so the butler session can dispatch workers.
	butlerMCPConfig string
}

type Status struct {
	Enabled      bool      `json:"enabled"`
	Connected    bool      `json:"connected"`
	HomeSiteID   string    `json:"homeSiteId,omitempty"`
	LastError    string    `json:"lastError,omitempty"`
	LastChangeAt time.Time `json:"lastChangeAt,omitempty"`
}

type transcriptSink interface {
	SendVoiceTranscript(ctx context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error)
	SendVoiceTranscriptStream(
		ctx context.Context,
		event openclaw.VoiceTranscriptEvent,
		onEvent func(openclaw.VoiceTranscriptStreamEvent),
	) (openclaw.VoiceTranscriptDelivery, error)
}

func New(cfg Config, sink transcriptSink, logger *obs.Logger, metrics *obs.Metrics) *Adapter {
	return &Adapter{
		cfg:     cfg,
		sink:    sink,
		log:     logger,
		metrics: metrics,
		dialer: &websocket.Dialer{
			HandshakeTimeout: cfg.HTTPTimeout,
		},
		status: Status{
			Enabled:      true,
			HomeSiteID:   cfg.HomeSiteID,
			LastChangeAt: time.Now(),
		},
	}
}

func (a *Adapter) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// SetRouter attaches the agent router so that chat.text requests with an
// explicit driver field are dispatched through the agent bus instead of the
// openclaw sink.
func (a *Adapter) SetRouter(r *agent.Router) { a.router = r }

// SetSessionManager attaches the logical-session table (ADR-014). When set,
// inbound sessionId fields prefixed "ls-" are resolved through the manager
// to the underlying CLI session id, and SESSION_NOT_FOUND retries write the
// new CLI id back. nil disables the manager-aware path entirely.
func (a *Adapter) SetSessionManager(m *logicalsession.Manager) { a.sessions = m }

// CwdPoolEntry is a name+path pair for the CWD pool, mirroring config.CwdEntry
// without importing the config package (to avoid circular deps).
type CwdPoolEntry struct {
	Name string
	Path string
}

// SetCwdPool attaches the configured CWD pool so cloud-proxied
// GET /v1/agent/cwd-pool requests can be served.
func (a *Adapter) SetCwdPool(pool []CwdPoolEntry) { a.cwdPool = pool }

// defaultStartCwd returns the best-fit fallback working directory for spawn
// paths that don't carry an explicit logical session (e.g. the voice
// transcript fan-out in handleChatTextViaAgent). Priority:
//  1. logical session manager's BBCLAW_DEFAULT_CWD
//  2. first CwdPool entry's path
//  3. "" — caller would then inherit the adapter process cwd, which is the
//     bug source (the /Volumes/.../adapter leak).
//
// Voice turns are one-shot (Start/Send/Stop per utterance) so there's no
// persisted logical session to consult — the pool order is the only signal
// about which project the operator considers "primary".
func (a *Adapter) defaultStartCwd() string {
	if a.sessions != nil {
		if cwd := a.sessions.DefaultCwd(); cwd != "" {
			return cwd
		}
	}
	if len(a.cwdPool) > 0 {
		return a.cwdPool[0].Path
	}
	return ""
}

// SetButlerWorkspace enables butler routing for the cloud voice path (ADR-021
// §1): when workspaceCwd is non-empty, the voice transcript fan-out routes
// every turn to the calling device's butler logical session (cwd=workspaceCwd,
// Role=butler, driver=claude-code) through the butler engine instead of the
// legacy one-shot driver spawn. mcpConfig is the butler's --mcp-config path
// (ADR-021 §2); empty disables dispatch. Empty workspaceCwd leaves the legacy
// voice path untouched.
func (a *Adapter) SetButlerWorkspace(workspaceCwd, mcpConfig string) {
	a.butlerWorkspace = strings.TrimSpace(workspaceCwd)
	a.butlerMCPConfig = strings.TrimSpace(mcpConfig)
}

// SetDriverState attaches the persistent driver-preference store, mirrored
// from the local HTTP layer so cloud-proxied agent turns honour the same
// active driver / active model selection as LAN-direct turns.
func (a *Adapter) SetDriverState(store *driverstate.Store) { a.driverState = store }

// resolveActiveModel mirrors httpapi.Server.resolveActiveModel for the cloud
// proxy path. Returns "" when no driverState store is wired.
func (a *Adapter) resolveActiveModel(driver string) string {
	if a.driverState == nil || driver == "" {
		return ""
	}
	return a.driverState.ActiveModel(driver)
}

func (a *Adapter) setStatus(connected bool, lastErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status.Enabled = true
	a.status.Connected = connected
	a.status.HomeSiteID = a.cfg.HomeSiteID
	if lastErr != nil {
		a.status.LastError = strings.TrimSpace(lastErr.Error())
	} else {
		a.status.LastError = ""
	}
	a.status.LastChangeAt = time.Now()
}

func (a *Adapter) Run(ctx context.Context) error {
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go a.pairingPollLoop(pollCtx)

	// Sweeper for cloud-proxied agent sessions (Phase 4.8). Cheap to run
	// even if no proxied request ever arrives — sweepAgentSessions is a
	// no-op until handleAgentMessageRequest lazily creates the registry.
	if a.router != nil {
		go a.runAgentSessionSweeper(pollCtx, agentSessionTTL)
	}

	dialURL, err := resolveCloudDialURL(a.cfg.CloudWSURL, a.cfg.HomeSiteID, a.cfg.CloudAuthToken)
	if err != nil {
		return err
	}
	a.log.Infof("home-adapter dial_url=%s home_site=%s", dialURL, a.cfg.HomeSiteID)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.runOnce(ctx, dialURL)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.setStatus(false, err)
		a.metrics.Inc("cloud_disconnect")
		a.log.Warnf("cloud disconnected home_site=%s err=%v reconnect_in=%s", a.cfg.HomeSiteID, err, a.cfg.ReconnectDelay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.cfg.ReconnectDelay):
		}
	}
}

func (a *Adapter) runOnce(ctx context.Context, dialURL string) error {
	conn, _, err := a.dialer.DialContext(ctx, dialURL, nil)
	if err != nil {
		a.metrics.Inc("cloud_dial_failed")
		a.setStatus(false, err)
		return fmt.Errorf("dial cloud ws: %w", err)
	}
	defer conn.Close()

	// All writes to conn must go through writeConn to satisfy gorilla's
	// "one concurrent writer" requirement.
	var writeMu sync.Mutex
	writeConn := func(env CloudEnvelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(env)
	}

	// ReadJSON blocks without ctx; closing the conn on cancel unblocks shutdown (Ctrl+C / SIGTERM).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	// Send periodic application-level JSON ping. WebSocket-level PING frames
	// can be absorbed by reverse proxies (nginx) and never reach the cloud
	// server, causing its 35s read-timeout to fire.
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := writeConn(CloudEnvelope{Type: "ping"}); err != nil {
					return
				}
			}
		}
	}()

	a.metrics.Inc("cloud_connected")
	a.setStatus(true, nil)
	a.log.Infof("cloud connected home_site=%s", a.cfg.HomeSiteID)

	for {
		var env CloudEnvelope
		if err := conn.ReadJSON(&env); err != nil {
			return fmt.Errorf("read cloud frame: %w", err)
		}
		a.metrics.Inc("cloud_message_in")

		switch strings.ToLower(strings.TrimSpace(env.Type)) {
		case "welcome", "ack":
			if strings.EqualFold(strings.TrimSpace(env.Type), "welcome") {
				if env.Payload != nil {
					if reg, ok := env.Payload["homeAdapterRegistration"].(string); ok {
						reg = strings.TrimSpace(reg)
						if reg != "" {
							a.log.Infof("cloud welcome home_site=%s home_adapter_registration=%s", a.cfg.HomeSiteID, reg)
						}
					}
				}
				// report adapter version info to cloud
				if err := writeConn(CloudEnvelope{
					Type:       "info",
					HomeSiteID: a.cfg.HomeSiteID,
					Payload: map[string]any{
						"adapterVersion": buildinfo.Tag,
						"buildTime":      buildinfo.BuildTime,
						"platform":       runtime.GOOS + "/" + runtime.GOARCH,
						"goVersion":      runtime.Version(),
					},
				}); err != nil {
					a.log.Warnf("send adapter info failed: %v", err)
				}
			}
			continue
		case "event":
			if strings.EqualFold(strings.TrimSpace(env.Kind), "registration.code") {
				code, _ := env.Payload["code"].(string)
				expiresAt, _ := env.Payload["expiresAt"].(string)
				a.announceRegistrationCode("ws", code, expiresAt)
			}
			continue
		case "request":
			go func(env CloudEnvelope) {
				if err := a.handleRequest(ctx, writeConn, env); err != nil {
					a.metrics.Inc("cloud_request_failed")
					if writeErr := a.writeErrorResponse(writeConn, env, err); writeErr != nil {
						a.log.Warnf("write error reply failed device=%s err=%v", env.DeviceID, writeErr)
					}
				}
			}(env)
		}
	}
}

func (a *Adapter) announceRegistrationCode(source, code, expiresAt string) {
	code = strings.TrimSpace(code)
	if code == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastRegCode == code {
		return
	}
	a.lastRegCode = code
	expiresAt = strings.TrimSpace(expiresAt)
	a.log.Infof("")
	a.log.Infof("╔══════════════════════════════════════════════════════════╗")
	a.log.Infof("║  PAIRING REQUIRED — this adapter is not yet claimed     ║")
	a.log.Infof("║                                                          ║")
	a.log.Infof("║  Code:  %s                                        ║", code)
	a.log.Infof("║  Expires: %s  ║", expiresAt)
	a.log.Infof("║                                                          ║")
	a.log.Infof("║  Go to BBClaw Cloud portal and claim this adapter:       ║")
	a.log.Infof("║    POST /v1/registrations/claim {\"code\":\"%s\"}     ║", code)
	a.log.Infof("╚══════════════════════════════════════════════════════════╝")
	a.log.Infof("")
}

func (a *Adapter) handleRequest(ctx context.Context, write func(CloudEnvelope) error, env CloudEnvelope) error {
	switch strings.ToLower(strings.TrimSpace(env.Kind)) {
	case "voice.transcript":
		return a.handleTranscriptRequest(ctx, write, env)
	case "chat.text":
		return a.handleChatTextRequest(ctx, write, env)
	case "chat.drivers":
		return a.handleChatDriversRequest(write, env)
	case "agent.drivers":
		// Phase 4.8 cloud agent proxy: cloud reverse-proxies firmware
		// /v1/agent/drivers requests through this kind.
		return a.handleAgentDriversRequest(write, env)
	case "agent.sessions":
		return a.handleAgentSessionsRequest(write, env)
	case "agent.sessions.create":
		// Phase B (ADR-014): cloud proxies firmware POST /v1/agent/sessions
		// through this kind so device-side "+ 新建 session" works in
		// cloud_saas mode the same as on LAN.
		return a.handleAgentSessionsCreateRequest(write, env)
	case "agent.sessions.update":
		// ADR-014 admin: cloud proxies web admin PATCH /v1/agent/sessions/{id}
		// for editing logical session title/cwd.
		return a.handleAgentSessionsUpdateRequest(write, env)
	case "agent.sessions.list.logical":
		// ADR-014 admin: cloud proxies web admin GET /v1/agent/sessions?kind=logical.
		return a.handleAgentSessionsListLogicalRequest(write, env)
	case "agent.sessions.delete":
		// ADR-014 admin: cloud proxies web admin DELETE /v1/agent/sessions/{id}.
		return a.handleAgentSessionsDeleteRequest(write, env)
	case "agent.cwd_pool":
		// Issue #30: cloud proxies firmware GET /v1/agent/cwd-pool through
		// this kind so device-side CWD picker works in cloud_saas mode.
		return a.handleAgentCwdPoolRequest(write, env)
	case "agent.active_driver.set":
		// Driver/model selection (this ADR): cloud proxies firmware
		// PUT /v1/agent/active_driver so the device settings UI can change
		// the active driver in cloud_saas mode.
		return a.handleAgentActiveDriverSetRequest(write, env)
	case "agent.active_model.set":
		// Driver/model selection (this ADR): cloud proxies firmware
		// PUT /v1/agent/drivers/{name}/active_model.
		return a.handleAgentActiveModelSetRequest(write, env)
	case "agent.messages":
		// Phase S3 cloud proxy: cloud reverse-proxies firmware
		// /v1/agent/sessions/{id}/messages history requests through this kind.
		return a.handleAgentMessagesRequest(write, env)
	case "agent.message":
		// Phase 4.8 cloud agent proxy: cloud reverse-proxies firmware
		// /v1/agent/message NDJSON streams through this kind.
		return a.handleAgentMessageRequest(ctx, write, env)
	case "agent.menu":
		// ADR-019: cloud proxies firmware GET /v1/agent/menu/{id} so the
		// server-driven menu renderer works in cloud_saas mode.
		return a.handleAgentMenuRequest(write, env)
	case "agent.menu.action":
		// ADR-019: cloud proxies firmware POST /v1/agent/menu/action.
		return a.handleAgentMenuActionRequest(write, env)
	default:
		return nil
	}
}

func (a *Adapter) handleChatDriversRequest(write func(CloudEnvelope) error, env CloudEnvelope) error {
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
		Kind:       "chat.drivers.reply",
		Payload:    map[string]any{"drivers": drivers},
	})
}

func (a *Adapter) handleChatTextRequest(ctx context.Context, write func(CloudEnvelope) error, env CloudEnvelope) error {
	text, _ := env.Payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("payload.text is required")
	}
	sessionKey, _ := env.Payload["sessionKey"].(string)
	streamID, _ := env.Payload["streamId"].(string)
	source, _ := env.Payload["source"].(string)
	nodeID, _ := env.Payload["nodeId"].(string)
	driverName, _ := env.Payload["driver"].(string)
	driverName = strings.TrimSpace(driverName)

	routeStart := time.Now()
	a.log.Infof("phase=chat_text_request_recv session=%s stream=%s text_chars=%d driver=%s",
		strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), utf8.RuneCountInString(text), driverName)

	if a.router != nil && driverName != "" {
		return a.handleChatTextViaAgent(ctx, write, env, text, sessionKey, streamID, driverName, routeStart)
	}

	writeEvent := func(kind string, payload map[string]any) {
		if err := a.writeStreamEvent(write, env, kind, payload); err != nil {
			a.log.Warnf("writeEvent failed kind=%s err=%v", kind, err)
		}
	}

	deltaSeq := 0
	delivery, err := a.sink.SendVoiceTranscriptStream(ctx, openclaw.VoiceTranscriptEvent{
		Text:       text,
		SessionKey: strings.TrimSpace(sessionKey),
		StreamID:   strings.TrimSpace(streamID),
		Source:     strings.TrimSpace(source),
		NodeID:     strings.TrimSpace(nodeID),
	}, func(evt openclaw.VoiceTranscriptStreamEvent) {
		switch evt.Type {
		case "reply.delta":
			if strings.TrimSpace(evt.Text) == "" {
				return
			}
			deltaSeq++
			a.log.Infof("phase=chat_reply_delta session=%s delta_seq=%d elapsed_s=%.3f text=%.80s",
				strings.TrimSpace(sessionKey), deltaSeq, time.Since(routeStart).Seconds(), evt.Text)
			writeEvent("voice.reply.delta", map[string]any{"text": evt.Text})
		case "thinking":
			writeEvent("thinking", map[string]any{"text": evt.Text})
		case "tool_call":
			writeEvent("tool_call", map[string]any{"name": evt.Text})
		}
	})
	if err != nil {
		a.log.Warnf("phase=chat_text_request_failed session=%s elapsed_s=%.3f err=%v",
			strings.TrimSpace(sessionKey), time.Since(routeStart).Seconds(), err)
		return err
	}
	replyText := strings.TrimSpace(delivery.ReplyText)
	a.log.Infof("phase=chat_text_request_done session=%s elapsed_s=%.3f reply_chars=%d",
		strings.TrimSpace(sessionKey), time.Since(routeStart).Seconds(), utf8.RuneCountInString(replyText))
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "voice.reply",
		Payload: map[string]any{
			"ok":   true,
			"text": replyText,
		},
	})
}

func (a *Adapter) handleTranscriptRequest(ctx context.Context, write func(CloudEnvelope) error, env CloudEnvelope) error {
	if strings.TrimSpace(env.DeviceID) == "" {
		return errors.New("deviceId is required")
	}
	text, _ := env.Payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("payload.text is required")
	}
	sessionKey, _ := env.Payload["sessionKey"].(string)
	streamID, _ := env.Payload["streamId"].(string)
	source, _ := env.Payload["source"].(string)
	nodeID, _ := env.Payload["nodeId"].(string)
	driverName, _ := env.Payload["driver"].(string)
	driverName = strings.TrimSpace(driverName)
	routeStart := time.Now()
	a.log.Infof("phase=transcript_request_recv device=%s session=%s stream=%s text_chars=%d driver=%s",
		env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), utf8.RuneCountInString(text), driverName)

	// Route through agent router if available. When driver is empty, use the
	// router's default driver instead of falling back to openclaw sink.
	if a.router != nil {
		if driverName == "" {
			driverName = a.router.DefaultName()
			a.log.Infof("phase=transcript_driver_default device=%s session=%s stream=%s driver=%s",
				env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), driverName)
		}
		return a.handleChatTextViaAgent(ctx, write, env, text, sessionKey, streamID, driverName, routeStart)
	}

	a.metrics.Inc("voice_transcript_forwarded")
	writeEvent := func(kind string, payload map[string]any) {
		if err := a.writeStreamEvent(write, env, kind, payload); err != nil {
			a.log.Warnf("writeEvent failed kind=%s device=%s err=%v", kind, env.DeviceID, err)
		}
	}
	writeEvent("voice.reply.status", map[string]any{"phase": "processing"})
	a.log.Infof("phase=openclaw_request_start device=%s session=%s stream=%s elapsed_s=0.000 text_chars=%d",
		env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), utf8.RuneCountInString(text))
	deltaSeq := 0
	delivery, err := a.sink.SendVoiceTranscriptStream(ctx, openclaw.VoiceTranscriptEvent{
		Text:       text,
		SessionKey: strings.TrimSpace(sessionKey),
		StreamID:   strings.TrimSpace(streamID),
		Source:     strings.TrimSpace(source),
		NodeID:     strings.TrimSpace(nodeID),
	}, func(evt openclaw.VoiceTranscriptStreamEvent) {
		switch evt.Type {
		case "reply.delta":
			if strings.TrimSpace(evt.Text) == "" {
				return
			}
			deltaSeq++
			a.log.Infof("phase=reply_delta_recv device=%s session=%s stream=%s delta_seq=%d text_chars=%d elapsed_s=%.3f text=%.80s",
				env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), deltaSeq,
				utf8.RuneCountInString(evt.Text), time.Since(routeStart).Seconds(), evt.Text)
			writeEvent("voice.reply.delta", map[string]any{"text": evt.Text})
			a.log.Infof("phase=reply_delta_sent device=%s session=%s stream=%s delta_seq=%d elapsed_s=%.3f",
				env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), deltaSeq,
				time.Since(routeStart).Seconds())
		case "thinking":
			a.log.Infof("phase=thinking_relay device=%s session=%s stream=%s", env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID))
			writeEvent("thinking", map[string]any{"text": evt.Text})
		case "tool_call":
			a.log.Infof("phase=tool_call_relay device=%s session=%s stream=%s tool=%s", env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), evt.Text)
			writeEvent("tool_call", map[string]any{"name": evt.Text})
		}
	})
	if err != nil {
		a.log.Warnf("phase=openclaw_request_failed device=%s session=%s stream=%s elapsed_s=%.3f err=%v",
			env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), time.Since(routeStart).Seconds(), err)
		return err
	}
	replyText := strings.TrimSpace(delivery.ReplyText)
	a.log.Infof("phase=transcript_request_done device=%s session=%s stream=%s elapsed_s=%.3f reply_chars=%d reply_wait_timed_out=%t",
		env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), time.Since(routeStart).Seconds(), utf8.RuneCountInString(replyText), delivery.ReplyWaitTimedOut)
	if err := write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		DeviceID:   env.DeviceID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "voice.reply",
		Payload: map[string]any{
			"ok":                true,
			"text":              replyText,
			"replyWaitTimedOut": delivery.ReplyWaitTimedOut,
		},
	}); err != nil {
		return fmt.Errorf("write reply: %w", err)
	}
	a.log.Infof("phase=voice_reply_sent device=%s session=%s stream=%s elapsed_s=%.3f reply_chars=%d",
		env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), time.Since(routeStart).Seconds(), utf8.RuneCountInString(replyText))
	a.metrics.Inc("voice_reply_sent")
	return nil
}

func (a *Adapter) handleChatTextViaAgent(
	ctx context.Context,
	write func(CloudEnvelope) error,
	env CloudEnvelope,
	text, sessionKey, streamID, driverName string,
	routeStart time.Time,
) error {
	// Butler routing (ADR-021 §1): when a butler workspace is configured, route
	// the voice turn to the device's butler logical session through the shared
	// butler engine (cwd=workspace, --mcp-config, warm-pool hit) instead of the
	// legacy one-shot spawn below. Falls back to the legacy path when the butler
	// can't be resolved so a transient session-store error never drops a turn.
	if a.butlerWorkspace != "" && a.sessions != nil {
		return a.handleChatTextViaButler(ctx, write, env, text, sessionKey, streamID, routeStart)
	}

	drv, ok := a.router.Get(driverName)
	if !ok {
		return fmt.Errorf("agent driver %q not registered", driverName)
	}

	// Voice turns don't carry a logical session id — fall back to the
	// configured default cwd so the spawned CLI doesn't inherit the adapter
	// process's own cwd (which leaks /Volumes/.../adapter into the model's
	// system prompt and confuses it about which project it's working in).
	startOpts := agent.StartOpts{Cwd: a.defaultStartCwd()}
	// Voice turns get the same butler device persona as session turns (ADR-018
	// §3) so the backend keeps replies short/speakable instead of leaking the
	// cwd or dumping walls of code to a 1.47" screen.
	startOpts.SystemPrompt = butler.DeviceSystemPrompt(startOpts.Cwd)
	sid, err := drv.Start(ctx, startOpts)
	if err != nil {
		return fmt.Errorf("agent start: %w", err)
	}
	a.log.Infof("phase=voice_agent_start driver=%s sid=%s cwd=%q device=%s",
		driverName, sid, startOpts.Cwd, env.DeviceID)
	defer func() { _ = drv.Stop(sid) }()

	events := drv.Events(sid)
	sendErrCh := make(chan error, 1)
	go func() { sendErrCh <- drv.Send(sid, text) }()

	writeEvent := func(kind string, payload map[string]any) {
		if err := a.writeStreamEvent(write, env, kind, payload); err != nil {
			a.log.Warnf("writeEvent failed kind=%s err=%v", kind, err)
		}
	}

	var replyParts []string
loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			switch ev.Type {
			case agent.EvText:
				if t := strings.TrimSpace(ev.Text); t != "" {
					replyParts = append(replyParts, ev.Text)
					writeEvent("voice.reply.delta", map[string]any{"text": ev.Text})
				}
			case agent.EvToolCall:
				if ev.Tool != nil {
					writeEvent("tool_call", map[string]any{"name": ev.Tool.Tool})
				}
			case agent.EvTurnEnd:
				break loop
			}
		}
	}

	if sendErr := <-sendErrCh; sendErr != nil {
		a.log.Warnf("phase=agent_send_failed driver=%s session=%s err=%v", driverName, sessionKey, sendErr)
	}

	replyText := strings.TrimSpace(strings.Join(replyParts, ""))
	a.log.Infof("phase=chat_text_request_done driver=%s session=%s elapsed_s=%.3f reply_chars=%d",
		driverName, sessionKey, time.Since(routeStart).Seconds(), utf8.RuneCountInString(replyText))
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "voice.reply",
		Payload:    map[string]any{"ok": true, "text": replyText},
	})
}

// handleChatTextViaButler runs one voice turn through the shared butler engine
// (ADR-021 §1). It resolves the device's butler logical session, runs the turn
// via butler.Engine.RunTurn, and re-emits the agent events as the same cloud
// frames the legacy voice path produced (voice.reply.delta / tool_call) so the
// cloud relay protocol is byte-for-byte compatible. The accumulated assistant
// text is sent as the final voice.reply envelope, exactly like before.
func (a *Adapter) handleChatTextViaButler(
	ctx context.Context,
	write func(CloudEnvelope) error,
	env CloudEnvelope,
	text, sessionKey, streamID string,
	routeStart time.Time,
) error {
	if a.agentSessions == nil {
		a.agentSessions = newAgentProxyRegistry()
	}

	requestedDriver := butler.ButlerDriver
	requestedSession := ""
	if bsess, err := a.sessions.EnsureButler(env.DeviceID, butler.ButlerDriver, a.butlerWorkspace); err != nil {
		// Non-fatal: without a butler session the engine still runs a fresh
		// claude-code turn in the workspace, just without session continuity.
		a.log.Warnf("butler: ensure failed device=%q err=%v; running butler turn without logical session", env.DeviceID, err)
	} else {
		requestedDriver = bsess.Driver
		requestedSession = string(bsess.ID)
	}

	sink := &voiceEventSink{a: a, write: write, env: env, streamID: streamID, sessionKey: sessionKey, routeStart: routeStart}

	eng := butler.NewEngine(butler.Deps{
		Router:   a.router,
		Sessions: a.sessions,
		Registry: &cloudSessionRegistry{r: a.agentSessions},
		Sink:     sink,
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
		ResolveActiveModel: a.resolveActiveModel,
		SystemPrompt:       butler.DeviceSystemPrompt,
		ButlerMCPConfig:    a.butlerMCPConfig,
		StartCtx:           ctx,
	})

	_, runErr := eng.RunTurn(ctx, butler.Request{
		Text:             text,
		RequestedDriver:  requestedDriver,
		RequestedSession: requestedSession,
		DeviceID:         env.DeviceID,
	})
	if runErr != nil {
		// ctx cancellation or a coded error after the stream committed: surface a
		// failed voice.reply so the cloud (and device) don't hang on the turn.
		a.log.Warnf("phase=voice_butler_failed device=%s session=%s elapsed_s=%.3f err=%v",
			env.DeviceID, strings.TrimSpace(sessionKey), time.Since(routeStart).Seconds(), runErr)
		return write(CloudEnvelope{
			Type:       "reply",
			MessageID:  env.MessageID,
			HomeSiteID: a.cfg.HomeSiteID,
			Kind:       "voice.reply",
			Payload:    map[string]any{"ok": false, "error": runErr.Error()},
		})
	}

	replyText := sink.replyText()
	a.log.Infof("phase=chat_text_request_done driver=%s session=%s elapsed_s=%.3f reply_chars=%d butler=true",
		requestedDriver, sessionKey, time.Since(routeStart).Seconds(), utf8.RuneCountInString(replyText))
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "voice.reply",
		Payload:    map[string]any{"ok": true, "text": replyText},
	})
}

// voiceEventSink adapts butler.EventSink to the cloud voice protocol frames.
// It maps EvText → voice.reply.delta and EvToolCall → tool_call (the exact
// kinds the legacy handleChatTextViaAgent loop emitted) and accumulates the
// assistant text for the final voice.reply envelope. Session frames are not
// part of the voice protocol, so EmitSession is a no-op. All methods return
// true: cloud streaming is best-effort and never aborts the turn on a dropped
// write (matching the legacy writeEvent behaviour). The engine calls these
// methods sequentially from its consume loop, so no locking is needed.
type voiceEventSink struct {
	a          *Adapter
	write      func(CloudEnvelope) error
	env        CloudEnvelope
	streamID   string
	sessionKey string
	routeStart time.Time

	parts    []string
	deltaSeq int
}

func (v *voiceEventSink) EmitSession(string, bool, string) bool { return true }

func (v *voiceEventSink) EmitEvent(ev agent.Event) bool {
	switch ev.Type {
	case agent.EvText:
		if t := strings.TrimSpace(ev.Text); t != "" {
			v.parts = append(v.parts, ev.Text)
			v.deltaSeq++
			v.a.log.Infof("phase=reply_delta_recv device=%s session=%s stream=%s delta_seq=%d text_chars=%d elapsed_s=%.3f",
				v.env.DeviceID, strings.TrimSpace(v.sessionKey), strings.TrimSpace(v.streamID), v.deltaSeq,
				utf8.RuneCountInString(ev.Text), time.Since(v.routeStart).Seconds())
			v.writeEvent("voice.reply.delta", map[string]any{"text": ev.Text})
		}
	case agent.EvToolCall:
		if ev.Tool != nil {
			v.writeEvent("tool_call", map[string]any{"name": ev.Tool.Tool})
		}
	}
	return true
}

func (v *voiceEventSink) EmitError(code, text string, _ bool) bool {
	v.a.log.Warnf("phase=voice_butler_error device=%s session=%s code=%s detail=%.120s",
		v.env.DeviceID, strings.TrimSpace(v.sessionKey), code, text)
	return true
}

func (v *voiceEventSink) writeEvent(kind string, payload map[string]any) {
	if err := v.a.writeStreamEvent(v.write, v.env, kind, payload); err != nil {
		v.a.log.Warnf("writeEvent failed kind=%s device=%s err=%v", kind, v.env.DeviceID, err)
	}
}

func (v *voiceEventSink) replyText() string {
	return strings.TrimSpace(strings.Join(v.parts, ""))
}

func (a *Adapter) writeStreamEvent(write func(CloudEnvelope) error, env CloudEnvelope, kind string, payload map[string]any) error {
	return write(CloudEnvelope{
		Type:       "event",
		MessageID:  env.MessageID,
		DeviceID:   env.DeviceID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       kind,
		Payload:    payload,
	})
}

func (a *Adapter) writeErrorResponse(write func(CloudEnvelope) error, env CloudEnvelope, err error) error {
	return write(CloudEnvelope{
		Type:       "reply",
		MessageID:  env.MessageID,
		DeviceID:   env.DeviceID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       "voice.reply",
		Payload: map[string]any{
			"ok":    false,
			"error": err.Error(),
		},
	})
}
