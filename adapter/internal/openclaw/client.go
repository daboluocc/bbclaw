package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// AgentParams holds the parameters for a method:"agent" request.
type AgentParams struct {
	// SessionKey is the openclaw session key (multi-turn continuity).
	SessionKey string
	// Text is the user message to send.
	Text string
	// StreamID is an optional per-turn idempotency key.
	StreamID string
	// Source identifies the caller (e.g. "bbclaw.adapter.agent").
	Source string
	// NodeID identifies this adapter node.
	NodeID string
}

// AgentStreamEvent is a single event received from the method:"agent" event stream.
type AgentStreamEvent struct {
	// Type is one of: "agent.delta", "agent.tool_call", "agent.tool_done",
	// "agent.thinking", "agent.done", or any other gateway-defined type.
	Type string
	// Text carries the text payload (delta text, tool name, etc.).
	Text string
}

// SendSlashCommand sends a slash command via the WS protocol using chat.send
// with operator role. Gateway parses /commands from chat.send when senderIsOwner.
func (c *Client) SendSlashCommand(ctx context.Context, command, sessionKey string) (string, error) {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "ws", "wss":
		return c.sendSlashCommandWS(ctx, command, sessionKey)
	default:
		return "", fmt.Errorf("slash command requires ws/wss endpoint, got %s", u.Scheme)
	}
}

func (c *Client) sendSlashCommandWS(ctx context.Context, command, sessionKey string) (string, error) {
	conn, _, err := c.dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("dial openclaw ws: %w", err)
	}
	defer conn.Close()

	nonce, err := c.waitConnectChallenge(conn)
	if err != nil {
		return "", err
	}

	// Operator connect with device identity — same keypair as node connections.
	// Uses cli client id + mode per official protocol docs.
	identity, err := loadOrCreateDeviceIdentity(c.deviceIdentityPath)
	if err != nil {
		return "", fmt.Errorf("load device identity: %w", err)
	}
	signedAtMs := time.Now().UnixMilli()
	scopes := []string{"operator.read", "operator.write"}
	scopesCSV := strings.Join(scopes, ",")
	_ = scopesCSV
	payload := buildDeviceAuthPayloadV3(deviceAuthPayloadV3{
		deviceID:     identity.DeviceID,
		clientID:     "cli",
		clientMode:   "cli",
		role:         "operator",
		scopes:       scopes,
		signedAtMs:   signedAtMs,
		token:        c.authToken,
		nonce:        nonce,
		platform:     runtime.GOOS,
		deviceFamily: "bbclaw",
	})
	signature := signDevicePayload(identity.PrivateKey, payload)

	connectParams := map[string]any{
		"minProtocol": 3,
		"maxProtocol": 3,
		"client": map[string]any{
			"id":           "cli",
			"version":      "bbclaw-adapter",
			"platform":     runtime.GOOS,
			"mode":         "cli",
			"deviceFamily": "bbclaw",
		},
		"role":   "operator",
		"scopes": scopes,
		"device": map[string]any{
			"id":        identity.DeviceID,
			"publicKey": identity.PublicKey,
			"signature": signature,
			"signedAt":  signedAtMs,
			"nonce":     nonce,
		},
	}
	if c.authToken != "" {
		connectParams["auth"] = map[string]any{"token": c.authToken}
	}

	connectReqID := "connect-" + uuid.NewString()
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     connectReqID,
		"method": "connect",
		"params": connectParams,
	}); err != nil {
		return "", err
	}
	if err := c.waitResponseOK(conn, connectReqID); err != nil {
		return "", fmt.Errorf("openclaw connect (operator) failed: %w", err)
	}

	// Subscribe to session messages so we can capture command output
	subReqID := "sub-" + uuid.NewString()
	_ = c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     subReqID,
		"method": "sessions.messages.subscribe",
		"params": map[string]any{"key": sessionKey},
	})
	_ = c.waitResponseOK(conn, subReqID)

	// Use chat.send to send the slash command — Gateway parses /commands from chat.send.
	chatReqID := "chat-" + uuid.NewString()
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     chatReqID,
		"method": "chat.send",
		"params": map[string]any{
			"sessionKey":     sessionKey,
			"message":        command,
			"idempotencyKey": chatReqID,
		},
	}); err != nil {
		return "", err
	}

	// Wait for response. Then try to capture any chat events that carry the
	// command output (e.g. /status results). Use a short window after the
	// response to collect streamed output.
	deadline := time.Now().Add(c.http.Timeout)
	var replyText string
	responseReceived := false
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return replyText, fmt.Errorf("set read deadline: %w", err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if responseReceived {
				return replyText, nil // timeout after response is OK
			}
			return "", fmt.Errorf("read response: %w", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}

		// Capture chat events (command output like /status results)
		if frame["type"] == "event" {
			replyText, _ = captureReplyFrame(frame, sessionKey, replyText, "", nil)
		}

		if frame["type"] != "res" {
			continue
		}
		respID, _ := frame["id"].(string)
		if respID != chatReqID {
			continue
		}
		ok, _ := frame["ok"].(bool)
		if !ok {
			if e, ok := frame["error"].(map[string]any); ok {
				code, _ := e["code"].(string)
				message, _ := e["message"].(string)
				return "", fmt.Errorf("code=%s message=%s", code, message)
			}
			return "", fmt.Errorf("chat.send failed")
		}
		// Check payload for inline text
		payload, _ := frame["payload"].(map[string]any)
		if payload != nil {
			if text, ok := payload["text"].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text), nil
			}
		}
		// Response received but command output may still arrive via chat events.
		// Wait a short window to collect it.
		responseReceived = true
		deadline = time.Now().Add(2 * time.Second)
	}
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
	// Gateway canonicalizes session keys to lowercase; normalize here to ensure
	// chat.subscribe and voice.transcript use the same casing.
	event.SessionKey = strings.ToLower(strings.TrimSpace(event.SessionKey))

	// Node connection: sends voice.transcript via node.event
	conn, _, err := c.dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return VoiceTranscriptDelivery{}, fmt.Errorf("dial openclaw ws: %w", err)
	}
	defer conn.Close()

	connectReqID := "connect-" + uuid.NewString()
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

	// Operator connection: subscribes to session events (session.tool, session.message)
	var opConn *websocket.Conn
	// opReplyCh receives the text reply forwarded from the operator connection when
	// the gateway sends it via session.message rather than via the node connection.
	opReplyCh := make(chan string, 1)
	waitChatReply := strings.TrimSpace(event.SessionKey) != ""
	if waitChatReply {
		oc, _, dialErr := c.dialer.DialContext(ctx, c.endpoint, nil)
		if dialErr == nil {
			opNonce, err := c.waitConnectChallenge(oc)
			if err == nil {
				opParams, err := c.buildOperatorConnectParams(opNonce)
				if err == nil {
					opConnID := "op-connect-" + uuid.NewString()
					_ = c.writeJSON(ctx, oc, map[string]any{
						"type": "req", "id": opConnID, "method": "connect", "params": opParams,
					})
					if c.waitResponseOK(oc, opConnID) == nil {
						subID := "op-sub-" + uuid.NewString()
						_ = c.writeJSON(ctx, oc, map[string]any{
							"type": "req", "id": subID, "method": "sessions.messages.subscribe",
							"params": map[string]any{"key": event.SessionKey},
						})
						_ = c.waitResponseOK(oc, subID)
						opConn = oc
						log.Printf("[openclaw] operator connection ready, forwarding session events for %s", event.SessionKey)
						// Forward session.tool events in background
						go c.forwardSessionToolEvents(ctx, opConn, event.SessionKey, onEvent, opReplyCh)
					}
				}
			}
			if opConn == nil {
				log.Printf("[openclaw] operator connection failed, no session.tool events")
				oc.Close()
			}
		}
	}
	if opConn != nil {
		defer opConn.Close()
	}

	// Subscribe to chat events on node connection
	if waitChatReply {
		chatSubscribeReqID := "chat-subscribe-" + uuid.NewString()
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
		// Race: wait for either the node-connection final reply OR the operator-
		// connection session.message text (whichever arrives first).
		type nodeResult struct {
			text    string
			timedOut bool
		}
		nodeCh := make(chan nodeResult, 1)
		go func() {
			text, timedOut := c.waitChatFinalText(ctx, conn, event.SessionKey, onEvent)
			nodeCh <- nodeResult{text, timedOut}
		}()

		select {
		case r := <-nodeCh:
			delivery.ReplyText = r.text
			delivery.ReplyWaitTimedOut = r.timedOut && strings.TrimSpace(r.text) == ""
		case text := <-opReplyCh:
			delivery.ReplyText = text
			// unblock the waitChatFinalText goroutine by closing its connection
			conn.Close()
		case <-ctx.Done():
			conn.Close()
		}
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

func (c *Client) buildOperatorConnectParams(nonce string) (map[string]any, error) {
	identity, err := loadOrCreateDeviceIdentity(c.deviceIdentityPath)
	if err != nil {
		return nil, fmt.Errorf("load/create device identity: %w", err)
	}
	signedAtMs := time.Now().UnixMilli()
	scopes := []string{"operator.read", "operator.write"}
	payload := buildDeviceAuthPayloadV3(deviceAuthPayloadV3{
		deviceID: identity.DeviceID, clientID: "cli", clientMode: "cli",
		role: "operator", scopes: scopes, signedAtMs: signedAtMs,
		token: c.authToken, nonce: nonce, platform: runtime.GOOS, deviceFamily: "bbclaw",
	})
	signature := signDevicePayload(identity.PrivateKey, payload)
	params := map[string]any{
		"minProtocol": 3, "maxProtocol": 3,
		"client": map[string]any{
			"id": "cli", "version": "bbclaw-adapter", "platform": runtime.GOOS,
			"mode": "cli", "deviceFamily": "bbclaw",
		},
		"role": "operator", "scopes": scopes,
		"device": map[string]any{
			"id": identity.DeviceID, "publicKey": identity.PublicKey,
			"signature": signature, "signedAt": signedAtMs, "nonce": nonce,
		},
	}
	if c.authToken != "" {
		params["auth"] = map[string]any{"token": c.authToken}
	}
	return params, nil
}

func (c *Client) forwardSessionToolEvents(ctx context.Context, conn *websocket.Conn, sessionKey string, onEvent func(VoiceTranscriptStreamEvent), replyCh chan<- string) {
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(c.http.Timeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		evtName, _ := frame["event"].(string)
		if evtName == "session.tool" {
			payload, _ := frame["payload"].(map[string]any)
			toolName, _ := payload["name"].(string)
			state, _ := payload["state"].(string)
			log.Printf("[openclaw] session.tool name=%s state=%s (session=%s)", toolName, state, sessionKey)
			if onEvent != nil && toolName != "" {
				onEvent(VoiceTranscriptStreamEvent{Type: "tool." + state, Text: toolName})
			}
		} else if evtName == "session.message" {
			payload, _ := frame["payload"].(map[string]any)
			message, _ := payload["message"].(map[string]any)
			role, _ := message["role"].(string)
			if content, ok := message["content"].([]any); ok && role == "assistant" {
				for _, item := range content {
					obj, _ := item.(map[string]any)
					itemType, _ := obj["type"].(string)
					switch itemType {
					case "thinking":
						text, _ := obj["thinking"].(string)
						if text != "" && onEvent != nil {
							log.Printf("[openclaw] thinking: %.80s", text)
							onEvent(VoiceTranscriptStreamEvent{Type: "thinking", Text: text})
						}
					case "toolCall":
						name, _ := obj["name"].(string)
						if name != "" && onEvent != nil {
							args, _ := json.Marshal(obj["arguments"])
							log.Printf("[openclaw] tool_call: %s %s", name, string(args))
							onEvent(VoiceTranscriptStreamEvent{Type: "tool_call", Text: name})
						}
					case "text":
						text, _ := obj["text"].(string)
						if text = strings.TrimSpace(text); text != "" {
							log.Printf("[openclaw] op reply text: %.80s", text)
							if onEvent != nil {
								onEvent(VoiceTranscriptStreamEvent{Type: "reply.delta", Text: text})
							}
							select {
							case replyCh <- text:
							default:
							}
						}
					}
				}
			}
		}
	}
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

		replyText, lastDeltaText = captureReplyFrame(frame, sessionKey, replyText, lastDeltaText, onEvent)

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

	idleTimeout := resolveReplyWaitTimeout(c.http.Timeout, c.replyWaitTimeout)
	deadline := time.Now().Add(idleTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	replyText := ""
	lastDeltaText := ""
	toolActive := false
	for {
		if ctx.Err() != nil {
			return replyText, false
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return replyText, false
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if isTimeoutErr(err) {
				if replyText != "" {
					return replyText, false
				}
				if strings.TrimSpace(lastDeltaText) != "" {
					return strings.TrimSpace(lastDeltaText), false
				}
				return "", true
			}
			if replyText == "" && strings.TrimSpace(lastDeltaText) != "" {
				return strings.TrimSpace(lastDeltaText), false
			}
			return replyText, false
		}
		// Any message from gateway means it's still alive — extend deadline
		deadline = time.Now().Add(idleTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}

		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		// Detect session.tool / session.message (thinking, toolCall) — agent still working
		if evtType, _ := frame["type"].(string); evtType == "event" {
			evtName, _ := frame["event"].(string)
			if evtName == "session.tool" {
				payload, _ := frame["payload"].(map[string]any)
				toolName, _ := payload["name"].(string)
				state, _ := payload["state"].(string)
				log.Printf("[openclaw] ws session.tool name=%s state=%s", toolName, state)
				if state == "running" || state == "pending" {
					toolActive = true
				} else {
					toolActive = false
				}
				if onEvent != nil && toolName != "" {
					onEvent(VoiceTranscriptStreamEvent{Type: "tool_call", Text: toolName})
				}
			} else if evtName == "session.message" {
				payload, _ := frame["payload"].(map[string]any)
				message, _ := payload["message"].(map[string]any)
				role, _ := message["role"].(string)
				if content, ok := message["content"].([]any); ok && role == "assistant" {
					for _, item := range content {
						obj, _ := item.(map[string]any)
						itemType, _ := obj["type"].(string)
						switch itemType {
						case "thinking":
							toolActive = true
							if text, _ := obj["thinking"].(string); text != "" && onEvent != nil {
								log.Printf("[openclaw] thinking: %.80s", text)
								onEvent(VoiceTranscriptStreamEvent{Type: "thinking", Text: text})
							}
						case "toolCall":
							toolActive = true
							if name, _ := obj["name"].(string); name != "" && onEvent != nil {
								log.Printf("[openclaw] tool_call: %s", name)
								onEvent(VoiceTranscriptStreamEvent{Type: "tool_call", Text: name})
							}
						}
					}
				}
			} else if evtName != "" && evtName != "chat" && evtName != "tick" && evtName != "health" {
				raw, _ := json.Marshal(frame)
				log.Printf("[openclaw] ws event=%s payload=%s", evtName, string(raw))
			}
		}
		prevReply := replyText
		replyText, lastDeltaText = captureReplyFrame(frame, sessionKey, replyText, lastDeltaText, onEvent)
		if replyText != "" && replyText != prevReply {
			// Got a final — but if agent is still working (tool/thinking active),
			// don't return yet; keep listening for subsequent messages.
			if !toolActive {
				return replyText, false
			}
			log.Printf("[openclaw] reply final received but agent still active, continuing to wait")
			toolActive = false // reset; will be set again if more tool/thinking events arrive
		}
	}
}

func captureReplyFrame(
	frame map[string]any,
	sessionKey string,
	replyText string,
	lastDeltaText string,
	onEvent func(VoiceTranscriptStreamEvent),
) (string, string) {
	state, text := parseChatText(frame, sessionKey)
	if state == "" {
		state, text = parseAgentText(frame, sessionKey)
	}
	switch state {
	case "delta":
		if onEvent != nil && text != "" && text != lastDeltaText {
			onEvent(VoiceTranscriptStreamEvent{Type: "reply.delta", Text: text})
			lastDeltaText = text
		}
	case "final":
		if text != "" {
			if replyText != "" && text != replyText && !strings.HasPrefix(text, replyText) {
				replyText = replyText + "\n" + text
			} else {
				replyText = text
			}
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
	if !sessionKeyMatches(payloadSessionKey, sessionKey) {
		return "", ""
	}
	state, _ := payload["state"].(string)
	if state != "final" && state != "delta" {
		return "", ""
	}
	return state, extractChatText(payload)
}

func parseAgentText(frame map[string]any, sessionKey string) (string, string) {
	if frame["type"] != "event" || frame["event"] != "agent" {
		return "", ""
	}
	payload, ok := frame["payload"].(map[string]any)
	if !ok {
		return "", ""
	}
	payloadSessionKey, _ := payload["sessionKey"].(string)
	if !sessionKeyMatches(payloadSessionKey, sessionKey) {
		return "", ""
	}
	stream, _ := payload["stream"].(string)
	if strings.TrimSpace(stream) != "assistant" {
		return "", ""
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		return "", ""
	}
	text, _ := data["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		delta, _ := data["delta"].(string)
		text = strings.TrimSpace(delta)
	}
	if text == "" {
		return "", ""
	}
	return "delta", text
}

func sessionKeyMatches(eventSessionKey, requestSessionKey string) bool {
	eventSessionKey = strings.ToLower(strings.TrimSpace(eventSessionKey))
	requestSessionKey = strings.ToLower(strings.TrimSpace(requestSessionKey))
	if eventSessionKey == "" || requestSessionKey == "" {
		return false
	}
	if eventSessionKey == requestSessionKey {
		return true
	}
	return sessionKeyTail(eventSessionKey) == sessionKeyTail(requestSessionKey)
}

func sessionKeyTail(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	if idx := strings.LastIndex(sessionKey, ":"); idx >= 0 && idx+1 < len(sessionKey) {
		return sessionKey[idx+1:]
	}
	return sessionKey
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

// SendAgentStream sends a method:"agent" request over the WS connection and
// streams the resulting events to onEvent. It uses an idle-based timeout
// (resolveReplyWaitTimeout) rather than a fixed HTTP timeout, so long-running
// tool chains (>30 s) are supported as long as the gateway keeps sending
// events.
//
// Event types forwarded to onEvent:
//
//	"agent.delta"     — incremental text from the assistant
//	"agent.tool_call" — tool invocation started
//	"agent.tool_done" — tool invocation finished
//	"agent.thinking"  — internal reasoning (callers may drop this)
//	"agent.done"      — turn complete; onEvent is called once then the method returns
//
// Any other event type is forwarded as-is so callers can handle future types.
func (c *Client) SendAgentStream(
	ctx context.Context,
	params AgentParams,
	onEvent func(AgentStreamEvent),
) error {
	// Normalize session key — gateway canonicalises to lowercase.
	params.SessionKey = strings.ToLower(strings.TrimSpace(params.SessionKey))

	conn, _, err := c.dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial openclaw ws: %w", err)
	}
	defer conn.Close()

	// ── auth handshake ──────────────────────────────────────────────────────
	nonce, err := c.waitConnectChallenge(conn)
	if err != nil {
		return err
	}
	connectParams, err := c.buildConnectParams(nonce)
	if err != nil {
		return err
	}
	connectReqID := "connect-" + uuid.NewString()
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     connectReqID,
		"method": "connect",
		"params": connectParams,
	}); err != nil {
		return err
	}
	if err := c.waitResponseOK(conn, connectReqID); err != nil {
		return fmt.Errorf("openclaw connect failed: %w", err)
	}

	// ── send method:"agent" request ─────────────────────────────────────────
	agentReqID := "agent-" + uuid.NewString()
	if err := c.writeJSON(ctx, conn, map[string]any{
		"type":   "req",
		"id":     agentReqID,
		"method": "agent",
		"params": map[string]any{
			"sessionKey": params.SessionKey,
			"text":       params.Text,
			"streamId":   params.StreamID,
			"source":     params.Source,
			"nodeId":     params.NodeID,
		},
	}); err != nil {
		return err
	}
	// Wait for the initial ack (ok:true) before entering the stream loop.
	if err := c.waitResponseOK(conn, agentReqID); err != nil {
		return fmt.Errorf("openclaw agent request failed: %w", err)
	}

	// ── stream event loop ───────────────────────────────────────────────────
	// Use idle timeout: any message from the gateway resets the deadline.
	idleTimeout := resolveAgentIdleTimeout(c.replyWaitTimeout)
	deadline := time.Now().Add(idleTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if isTimeoutErr(err) {
				return fmt.Errorf("openclaw agent stream idle timeout after %s", idleTimeout)
			}
			return fmt.Errorf("read agent stream: %w", err)
		}
		// Any message resets the idle deadline.
		deadline = time.Now().Add(idleTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}

		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}

		// We expect event frames: {"type":"event","event":"agent","payload":{...}}
		if frame["type"] != "event" || frame["event"] != "agent" {
			continue
		}
		payload, _ := frame["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		// Verify session key matches.
		payloadSessionKey, _ := payload["sessionKey"].(string)
		if !sessionKeyMatches(payloadSessionKey, params.SessionKey) {
			continue
		}

		evtType, _ := payload["type"].(string)
		evtType = strings.TrimSpace(evtType)
		if evtType == "" {
			continue
		}

		// Extract text from common payload shapes.
		text := extractAgentEventText(payload)

		if onEvent != nil {
			onEvent(AgentStreamEvent{Type: evtType, Text: text})
		}

		if evtType == "agent.done" {
			return nil
		}
	}
}

// extractAgentEventText pulls the text value out of an agent event payload.
// The gateway may use "text", "delta", or "name" depending on event type.
func extractAgentEventText(payload map[string]any) string {
	for _, key := range []string{"text", "delta", "name"} {
		if v, ok := payload[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// Nested data object (some gateway versions wrap in data:{text:...})
	if data, ok := payload["data"].(map[string]any); ok {
		for _, key := range []string{"text", "delta", "name"} {
			if v, ok := data[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// SendChatAbort sends a chat.abort request to the gateway for the given
// sessionKey. This notifies the server to cancel any in-progress agent turn.
// It is best-effort: errors are logged but not returned to the caller.
//
// SendChatAbort opens its own short-lived WS connection so it can be called
// from Stop() independently of the Send() connection.
func (c *Client) SendChatAbort(ctx context.Context, sessionKey string) error {
	sessionKey = strings.ToLower(strings.TrimSpace(sessionKey))
	if sessionKey == "" {
		return nil
	}

	// Use a short timeout for the abort — we don't want Stop() to block.
	abortTimeout := 5 * time.Second
	dialCtx, cancel := context.WithTimeout(ctx, abortTimeout)
	defer cancel()

	conn, _, err := c.dialer.DialContext(dialCtx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial openclaw ws for abort: %w", err)
	}
	defer conn.Close()

	nonce, err := c.waitConnectChallenge(conn)
	if err != nil {
		return fmt.Errorf("abort connect challenge: %w", err)
	}
	// Use operator role for abort — same as slash command path.
	connectParams, err := c.buildOperatorConnectParams(nonce)
	if err != nil {
		return fmt.Errorf("abort build connect params: %w", err)
	}
	connectReqID := "abort-connect-" + uuid.NewString()
	if err := c.writeJSON(dialCtx, conn, map[string]any{
		"type":   "req",
		"id":     connectReqID,
		"method": "connect",
		"params": connectParams,
	}); err != nil {
		return fmt.Errorf("abort connect write: %w", err)
	}
	if err := c.waitResponseOK(conn, connectReqID); err != nil {
		return fmt.Errorf("abort connect failed: %w", err)
	}

	abortReqID := "abort-" + uuid.NewString()
	if err := c.writeJSON(dialCtx, conn, map[string]any{
		"type":   "req",
		"id":     abortReqID,
		"method": "chat.abort",
		"params": map[string]any{
			"sessionKey": sessionKey,
		},
	}); err != nil {
		return fmt.Errorf("abort write: %w", err)
	}
	// Best-effort wait for ack — ignore errors (gateway may not support abort yet).
	_ = c.waitResponseOK(conn, abortReqID)
	return nil
}

// resolveAgentIdleTimeout returns the idle timeout for the agent event stream.
// It is more generous than the HTTP timeout to support long tool chains.
func resolveAgentIdleTimeout(override time.Duration) time.Duration {
	const defaultIdle = 120 * time.Second
	const maxIdle = 300 * time.Second
	if override > 0 {
		if override > maxIdle {
			return maxIdle
		}
		return override
	}
	return defaultIdle
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
