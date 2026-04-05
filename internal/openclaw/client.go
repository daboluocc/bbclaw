package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type VoiceTranscriptEvent struct {
	Text       string
	SessionKey string
	StreamID   string
	Source     string
	NodeID     string
}

type VoiceTranscriptDelivery struct {
	ReplyText         string
	ReplyWaitTimedOut bool
}

type VoiceTranscriptStreamEvent struct {
	Type string
	Text string
}

// SendAgentMessage sends a chat message via the "agent" RPC method.
// This is used for slash commands (/stop, /new, /status) which must
// go through the chat path for Gateway command parsing.
func (c *Client) SendAgentMessage(ctx context.Context, message, sessionKey string) error {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return fmt.Errorf("parse openclaw endpoint: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "ws", "wss":
		return c.sendAgentMessageWS(ctx, message, sessionKey)
	default:
		return fmt.Errorf("agent message requires ws/wss endpoint, got %s", u.Scheme)
	}
}

func (c *Client) sendAgentMessageWS(ctx context.Context, message, sessionKey string) error {
	conn, _, err := c.dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial openclaw ws: %w", err)
	}
	defer conn.Close()

	// agent method requires role:"control", not role:"node".
	connectReqID := "connect-" + uuid.NewString()
	connectParams := map[string]any{
		"minProtocol": 3,
		"maxProtocol": 3,
		"client": map[string]any{
			"id":       "control-ui",
			"name":     "bbclaw-adapter",
			"version":  "1.0.0",
			"platform": runtime.GOOS,
		},
		"role": "control",
	}
	if c.authToken != "" {
		connectParams["auth"] = map[string]any{"token": c.authToken}
	}
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     connectReqID,
		"method": "connect",
		"params": connectParams,
	}); err != nil {
		return err
	}
	if err := c.waitResponseOK(conn, connectReqID); err != nil {
		return fmt.Errorf("openclaw connect (control) failed: %w", err)
	}

	agentReqID := "agent-" + uuid.NewString()
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     agentReqID,
		"method": "agent",
		"params": map[string]any{
			"message":    message,
			"sessionKey": sessionKey,
		},
	}); err != nil {
		return err
	}
	return c.waitResponseOK(conn, agentReqID)
}

type Options struct {
	NodeID             string
	AuthToken          string
	DeviceIdentityPath string
	ReplyWaitTimeout   time.Duration
}

type Client struct {
	endpoint           string
	nodeID             string
	authToken          string
	deviceIdentityPath string
	replyWaitTimeout   time.Duration
	http               *http.Client
	dialer             *websocket.Dialer
}

func NewClient(endpoint string, timeout time.Duration, opts ...Options) *Client {
	option := Options{}
	if len(opts) > 0 {
		option = opts[0]
	}
	return &Client{
		endpoint:           endpoint,
		nodeID:             strings.TrimSpace(option.NodeID),
		authToken:          strings.TrimSpace(option.AuthToken),
		deviceIdentityPath: strings.TrimSpace(option.DeviceIdentityPath),
		replyWaitTimeout:   option.ReplyWaitTimeout,
		http: &http.Client{
			Timeout: timeout,
		},
		dialer: &websocket.Dialer{
			HandshakeTimeout: timeout,
		},
	}
}

func (c *Client) SendVoiceTranscript(ctx context.Context, event VoiceTranscriptEvent) (VoiceTranscriptDelivery, error) {
	return c.sendVoiceTranscriptWithStream(ctx, event, nil)
}

func (c *Client) SendVoiceTranscriptStream(
	ctx context.Context,
	event VoiceTranscriptEvent,
	onEvent func(VoiceTranscriptStreamEvent),
) (VoiceTranscriptDelivery, error) {
	return c.sendVoiceTranscriptWithStream(ctx, event, onEvent)
}

func (c *Client) sendVoiceTranscriptWithStream(
	ctx context.Context,
	event VoiceTranscriptEvent,
	onEvent func(VoiceTranscriptStreamEvent),
) (VoiceTranscriptDelivery, error) {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("parse openclaw endpoint: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "ws", "wss":
		return c.sendVoiceTranscriptWS(ctx, event, onEvent)
	case "http", "https":
		delivery, err := c.sendVoiceTranscriptHTTP(ctx, event)
		if err != nil {
			return VoiceTranscriptDelivery{}, err
		}
		if onEvent != nil && strings.TrimSpace(delivery.ReplyText) != "" {
			onEvent(VoiceTranscriptStreamEvent{Type: "reply.delta", Text: delivery.ReplyText})
		}
		return delivery, nil
	default:
		return VoiceTranscriptDelivery{}, fmt.Errorf("unsupported openclaw endpoint scheme: %s", u.Scheme)
	}
}

func (c *Client) sendVoiceTranscriptHTTP(ctx context.Context, event VoiceTranscriptEvent) (VoiceTranscriptDelivery, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      "bbclaw-adapter",
		"method":  "node.event",
		"params": map[string]any{
			"event": "voice.transcript",
			"payload": map[string]any{
				"text":       event.Text,
				"sessionKey": event.SessionKey,
				"streamId":   event.StreamID,
				"source":     event.Source,
				"nodeId":     event.NodeID,
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("call openclaw rpc: %w", err)
	}
	defer resp.Body.Close()

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("read openclaw response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return VoiceTranscriptDelivery{}, fmt.Errorf("openclaw rpc status=%d body=%s", resp.StatusCode, string(rb))
	}

	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rb, &rpcResp); err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("decode openclaw rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("openclaw rpc error code=%d message=%s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return VoiceTranscriptDelivery{}, nil
}

func (c *Client) sendVoiceTranscriptWS(
	ctx context.Context,
	event VoiceTranscriptEvent,
	onEvent func(VoiceTranscriptStreamEvent),
) (VoiceTranscriptDelivery, error) {
	conn, _, err := c.dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("dial openclaw ws: %w", err)
	}
	defer conn.Close()

	connectReqID := "connect-" + uuid.NewString()
	chatSubscribeReqID := "chat-subscribe-" + uuid.NewString()
	nodeEventReqID := "node-event-" + uuid.NewString()
	delivery := VoiceTranscriptDelivery{}

	nonce, err := c.waitConnectChallenge(conn)
	if err != nil {
		return delivery, err
	}
	connectParams, err := c.buildConnectParams(nonce)
	if err != nil {
		return delivery, err
	}
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     connectReqID,
		"method": "connect",
		"params": connectParams,
	}); err != nil {
		return delivery, err
	}
	if err := c.waitResponseOK(conn, connectReqID); err != nil {
		return delivery, fmt.Errorf("openclaw connect failed: %w", err)
	}

	waitChatReply := strings.TrimSpace(event.SessionKey) != ""
	if waitChatReply {
		if err := c.writeJSON(ctx, conn, map[string]any{
			"type":   "req",
			"id":     chatSubscribeReqID,
			"method": "node.event",
			"params": map[string]any{
				"event": "chat.subscribe",
				"payload": map[string]any{
					"sessionKey": event.SessionKey,
				},
			},
		}); err != nil {
			return delivery, err
		}
		if err := c.waitResponseOK(conn, chatSubscribeReqID); err != nil {
			waitChatReply = false
		}
	}

	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     nodeEventReqID,
		"method": "node.event",
		"params": map[string]any{
			"event": "voice.transcript",
			"payload": map[string]any{
				"text":       event.Text,
				"sessionKey": event.SessionKey,
				"streamId":   event.StreamID,
				"source":     event.Source,
				"nodeId":     event.NodeID,
			},
		},
	}); err != nil {
		return delivery, err
	}
	initialReplyText, err := c.waitResponseOKWithChatCapture(conn, nodeEventReqID, event.SessionKey, onEvent)
	if err != nil {
		return delivery, fmt.Errorf("openclaw node.event failed: %w", err)
	}
	if initialReplyText != "" {
		delivery.ReplyText = initialReplyText
	}
	if waitChatReply && strings.TrimSpace(delivery.ReplyText) == "" {
		replyText, timedOut := c.waitChatFinalText(ctx, conn, event.SessionKey, onEvent)
		delivery.ReplyText = replyText
		delivery.ReplyWaitTimedOut = timedOut && strings.TrimSpace(replyText) == ""
	}
	return delivery, nil
}

func (c *Client) buildConnectParams(nonce string) (map[string]any, error) {
	identity, err := loadOrCreateDeviceIdentity(c.deviceIdentityPath)
	if err != nil {
		return nil, fmt.Errorf("load/create device identity: %w", err)
	}
	signedAtMs := time.Now().UnixMilli()
	signatureToken := c.authToken
	payload := buildDeviceAuthPayloadV3(deviceAuthPayloadV3{
		deviceID:     identity.DeviceID,
		clientID:     "node-host",
		clientMode:   "node",
		role:         "node",
		scopes:       nil,
		signedAtMs:   signedAtMs,
		token:        signatureToken,
		nonce:        nonce,
		platform:     runtime.GOOS,
		deviceFamily: "bbclaw",
	})
	signature := signDevicePayload(identity.PrivateKey, payload)

	auth := map[string]any{}
	if c.authToken != "" {
		auth["token"] = c.authToken
	}
	params := map[string]any{
		"minProtocol": 3,
		"maxProtocol": 3,
		"client": map[string]any{
			"id":           "node-host",
			"displayName":  c.resolveNodeID(),
			"version":      "bbclaw-adapter",
			"platform":     runtime.GOOS,
			"deviceFamily": "bbclaw",
			"mode":         "node",
			"instanceId":   c.resolveNodeID(),
		},
		"role": "node",
		"caps": []string{},
		"device": map[string]any{
			"id":        identity.DeviceID,
			"publicKey": identity.PublicKey,
			"signature": signature,
			"signedAt":  signedAtMs,
			"nonce":     nonce,
		},
	}
	if len(auth) > 0 {
		params["auth"] = auth
	}
	return params, nil
}

func (c *Client) resolveNodeID() string {
	if c.nodeID != "" {
		return c.nodeID
	}
	return "bbclaw-adapter"
}

func (c *Client) waitConnectChallenge(conn *websocket.Conn) (string, error) {
	deadline := time.Now().Add(c.http.Timeout)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return "", fmt.Errorf("set read deadline: %w", err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return "", fmt.Errorf("read connect challenge: %w", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		if frame["type"] != "event" || frame["event"] != "connect.challenge" {
			continue
		}
		payload, _ := frame["payload"].(map[string]any)
		nonce, _ := payload["nonce"].(string)
		nonce = strings.TrimSpace(nonce)
		if nonce == "" {
			return "", fmt.Errorf("connect challenge missing nonce")
		}
		return nonce, nil
	}
}

func (c *Client) waitResponseOK(conn *websocket.Conn, id string) error {
	deadline := time.Now().Add(c.http.Timeout)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		if frame["type"] != "res" {
			continue
		}
		respID, _ := frame["id"].(string)
		if respID != id {
			continue
		}
		ok, _ := frame["ok"].(bool)
		if ok {
			return nil
		}
		if e, ok := frame["error"].(map[string]any); ok {
			code, _ := e["code"].(string)
			message, _ := e["message"].(string)
			return fmt.Errorf("code=%s message=%s", code, message)
		}
		return fmt.Errorf("unknown response error")
	}
}

func (c *Client) waitResponseOKWithChatCapture(
	conn *websocket.Conn,
	id string,
	sessionKey string,
	onEvent func(VoiceTranscriptStreamEvent),
) (string, error) {
	deadline := time.Now().Add(c.http.Timeout)
	replyText := ""
	lastDeltaText := ""
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return replyText, fmt.Errorf("set read deadline: %w", err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return replyText, fmt.Errorf("read response: %w", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}

		replyText, lastDeltaText = captureChatFrame(frame, sessionKey, replyText, lastDeltaText, onEvent)

		if frame["type"] != "res" {
			continue
		}
		respID, _ := frame["id"].(string)
		if respID != id {
			continue
		}
		ok, _ := frame["ok"].(bool)
		if ok {
			return replyText, nil
		}
		if e, ok := frame["error"].(map[string]any); ok {
			code, _ := e["code"].(string)
			message, _ := e["message"].(string)
			return replyText, fmt.Errorf("code=%s message=%s", code, message)
		}
		return replyText, fmt.Errorf("unknown response error")
	}
}

func (c *Client) waitChatFinalText(
	ctx context.Context,
	conn *websocket.Conn,
	sessionKey string,
	onEvent func(VoiceTranscriptStreamEvent),
) (string, bool) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return "", false
	}

	deadline := time.Now().Add(resolveReplyWaitTimeout(c.http.Timeout, c.replyWaitTimeout))
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	replyText := ""
	lastDeltaText := ""
	for {
		if ctx.Err() != nil {
			return "", false
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return "", false
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if isTimeoutErr(err) {
				return "", true
			}
			return "", false
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		replyText, lastDeltaText = captureChatFrame(frame, sessionKey, replyText, lastDeltaText, onEvent)
		if replyText != "" {
			return replyText, false
		}
	}
}

func captureChatFrame(
	frame map[string]any,
	sessionKey string,
	replyText string,
	lastDeltaText string,
	onEvent func(VoiceTranscriptStreamEvent),
) (string, string) {
	state, text := parseChatText(frame, sessionKey)
	switch state {
	case "delta":
		if onEvent != nil && text != "" && text != lastDeltaText {
			onEvent(VoiceTranscriptStreamEvent{Type: "reply.delta", Text: text})
			lastDeltaText = text
		}
	case "final":
		if text != "" {
			replyText = text
			if onEvent != nil && lastDeltaText != "" && text != lastDeltaText {
				onEvent(VoiceTranscriptStreamEvent{Type: "reply.delta", Text: text})
				lastDeltaText = text
			}
		}
	}
	return replyText, lastDeltaText
}

func parseChatText(frame map[string]any, sessionKey string) (string, string) {
	if frame["type"] != "event" || frame["event"] != "chat" {
		return "", ""
	}
	payload, ok := frame["payload"].(map[string]any)
	if !ok {
		return "", ""
	}
	payloadSessionKey, _ := payload["sessionKey"].(string)
	if strings.TrimSpace(payloadSessionKey) != strings.TrimSpace(sessionKey) {
		return "", ""
	}
	state, _ := payload["state"].(string)
	if state != "final" && state != "delta" {
		return "", ""
	}
	return state, extractChatText(payload)
}

func extractChatText(payload map[string]any) string {
	message, ok := payload["message"].(map[string]any)
	if !ok {
		return ""
	}
	if text, ok := message["text"].(string); ok {
		return strings.TrimSpace(text)
	}
	content, ok := message["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range content {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := obj["type"].(string)
		text, _ := obj["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if itemType == "" || itemType == "text" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func isTimeoutErr(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func resolveReplyWaitTimeout(base, override time.Duration) time.Duration {
	const maxWait = 120 * time.Second
	if override > 0 {
		base = override
	}
	if base <= 0 {
		return 25 * time.Second
	}
	if base < maxWait {
		return base
	}
	return maxWait
}

func (c *Client) writeJSON(ctx context.Context, conn *websocket.Conn, payload any) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(c.http.Timeout))
	}
	if err := conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("write websocket frame: %w", err)
	}
	return nil
}
