package homeadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/obs"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/openclaw"
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
	}
}

func (a *Adapter) Run(ctx context.Context) error {
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
	a.log.Infof("cloud connected home_site=%s", a.cfg.HomeSiteID)

	for {
		var env CloudEnvelope
		if err := conn.ReadJSON(&env); err != nil {
			return fmt.Errorf("read cloud frame: %w", err)
		}
		a.metrics.Inc("cloud_message_in")

		switch strings.ToLower(strings.TrimSpace(env.Type)) {
		case "welcome", "ack":
			continue
		case "ping":
			if err := conn.WriteJSON(CloudEnvelope{Type: "pong"}); err != nil {
				return fmt.Errorf("write pong: %w", err)
			}
			continue
		case "request":
			if err := a.handleRequest(ctx, conn, env); err != nil {
				a.metrics.Inc("cloud_request_failed")
				if writeErr := a.writeErrorResponse(conn, env, err); writeErr != nil {
					return fmt.Errorf("write error reply: %w", writeErr)
				}
				continue
			}
		}
	}
}

func (a *Adapter) handleRequest(ctx context.Context, conn *websocket.Conn, env CloudEnvelope) error {
	switch strings.ToLower(strings.TrimSpace(env.Kind)) {
	case "voice.transcript":
		return a.handleTranscriptRequest(ctx, conn, env)
	default:
		return nil
	}
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
	a.log.Infof("voice.transcript request device=%s session=%s stream=%s text_chars=%d",
		env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), utf8.RuneCountInString(text))

	a.metrics.Inc("voice_transcript_forwarded")
	_ = a.writeStreamEvent(conn, env, "voice.reply.status", map[string]any{
		"phase": "processing",
	})
	delivery, err := a.sink.SendVoiceTranscriptStream(ctx, openclaw.VoiceTranscriptEvent{
		Text:       text,
		SessionKey: strings.TrimSpace(sessionKey),
		StreamID:   strings.TrimSpace(streamID),
		Source:     strings.TrimSpace(source),
		NodeID:     strings.TrimSpace(nodeID),
	}, func(evt openclaw.VoiceTranscriptStreamEvent) {
		if evt.Type != "reply.delta" || strings.TrimSpace(evt.Text) == "" {
			return
		}
		if writeErr := a.writeStreamEvent(conn, env, "voice.reply.delta", map[string]any{
			"text": evt.Text,
		}); writeErr != nil {
			a.log.Warnf("voice.reply.delta failed device=%s session=%s stream=%s err=%v",
				env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), writeErr)
		}
	})
	if err != nil {
		a.log.Warnf("voice.transcript failed device=%s session=%s stream=%s elapsed_s=%.3f err=%v",
			env.DeviceID, strings.TrimSpace(sessionKey), strings.TrimSpace(streamID), time.Since(routeStart).Seconds(), err)
		return err
	}
	replyText := strings.TrimSpace(delivery.ReplyText)
	a.log.Infof("voice.transcript reply device=%s session=%s stream=%s elapsed_s=%.3f reply_chars=%d reply_wait_timed_out=%t",
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
