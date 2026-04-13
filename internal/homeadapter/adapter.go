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

	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
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
	log     *obs.Logger
	metrics *obs.Metrics
	dialer  *websocket.Dialer

	mu          sync.Mutex
	lastRegCode string
	status      Status
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
				if err := conn.WriteJSON(CloudEnvelope{
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
		case "ping":
			if err := conn.WriteJSON(CloudEnvelope{Type: "pong"}); err != nil {
				return fmt.Errorf("write pong: %w", err)
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
				if err := a.handleRequest(ctx, conn, env); err != nil {
					a.metrics.Inc("cloud_request_failed")
					if writeErr := a.writeErrorResponse(conn, env, err); writeErr != nil {
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
	a.log.Infof("home-adapter registration claim_required code=%s expires_at=%s home_site=%s source=%s — claim in BBClaw Cloud portal: POST /v1/registrations/claim",
		code, expiresAt, a.cfg.HomeSiteID, source)
}

func (a *Adapter) handleRequest(ctx context.Context, conn *websocket.Conn, env CloudEnvelope) error {
	switch strings.ToLower(strings.TrimSpace(env.Kind)) {
	case "voice.transcript":
		return a.handleTranscriptRequest(ctx, conn, env)
	case "chat.text":
		return a.handleChatTextRequest(ctx, conn, env)
	default:
		return nil
	}
}

func (a *Adapter) handleChatTextRequest(ctx context.Context, conn *websocket.Conn, env CloudEnvelope) error {
	text, _ := env.Payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("payload.text is required")
	}
	sessionKey, _ := env.Payload["sessionKey"].(string)
	streamID, _ := env.Payload["streamId"].(string)
	source, _ := env.Payload["source"].(string)
	nodeID, _ := env.Payload["nodeId"].(string)

	routeStart := time.Now()
	a.log.Infof("phase=chat_text_request_recv session=%s stream=%s text_chars=%d",
		strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), utf8.RuneCountInString(text))

	var connMu sync.Mutex
	writeEvent := func(kind string, payload map[string]any) {
		connMu.Lock()
		defer connMu.Unlock()
		if err := a.writeStreamEvent(conn, env, kind, payload); err != nil {
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
	return conn.WriteJSON(CloudEnvelope{
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

func (a *Adapter) handleTranscriptRequest(ctx context.Context, conn *websocket.Conn, env CloudEnvelope) error {
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
	routeStart := time.Now()
	a.log.Infof("phase=transcript_request_recv device=%s session=%s stream=%s text_chars=%d",
		env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), utf8.RuneCountInString(text))

	a.metrics.Inc("voice_transcript_forwarded")
	var connMu sync.Mutex
	writeEvent := func(kind string, payload map[string]any) {
		connMu.Lock()
		defer connMu.Unlock()
		if err := a.writeStreamEvent(conn, env, kind, payload); err != nil {
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
	if err := conn.WriteJSON(CloudEnvelope{
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

func (a *Adapter) writeStreamEvent(conn *websocket.Conn, env CloudEnvelope, kind string, payload map[string]any) error {
	if conn == nil {
		return nil
	}
	return conn.WriteJSON(CloudEnvelope{
		Type:       "event",
		MessageID:  env.MessageID,
		DeviceID:   env.DeviceID,
		HomeSiteID: a.cfg.HomeSiteID,
		Kind:       kind,
		Payload:    payload,
	})
}

func (a *Adapter) writeErrorResponse(conn *websocket.Conn, env CloudEnvelope, err error) error {
	return conn.WriteJSON(CloudEnvelope{
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
