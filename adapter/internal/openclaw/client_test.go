package openclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSendVoiceTranscriptStreamWS_CapturesDeltaAndFinal(t *testing.T) {
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.WriteJSON(map[string]any{
			"type": "event", "event": "connect.challenge",
			"payload": map[string]any{"nonce": "n-1"},
		})

		// Respond OK to all requests; send chat events after voice.transcript
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			reqID, _ := req["id"].(string)
			_ = conn.WriteJSON(map[string]any{"type": "res", "id": reqID, "ok": true})
			if method, _ := req["method"].(string); method == "node.event" {
				if params, ok := req["params"].(map[string]any); ok && params["event"] == "voice.transcript" {
					break
				}
			}
		}

		events := []map[string]any{
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]any{
					"sessionKey": "agent:main:main",
					"state":      "delta",
					"message": map[string]any{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "text", "text": "Hello"},
						},
					},
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]any{
					"sessionKey": "agent:main:main",
					"state":      "delta",
					"message": map[string]any{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "text", "text": "Hello"},
						},
					},
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]any{
					"sessionKey": "agent:main:main",
					"state":      "delta",
					"message": map[string]any{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "text", "text": "Hello world"},
						},
					},
				},
			},
			{
				"type":  "event",
				"event": "chat",
				"payload": map[string]any{
					"sessionKey": "agent:main:main",
					"state":      "final",
					"message": map[string]any{
						"role": "assistant",
						"content": []map[string]any{
							{"type": "text", "text": "Hello world!"},
						},
					},
				},
			},
		}
		for _, evt := range events {
			if err := conn.WriteJSON(evt); err != nil {
				t.Fatalf("write chat event: %v", err)
			}
		}
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	u.Scheme = "ws"

	c := NewClient(u.String(), 2*time.Second, Options{
		NodeID:             "bbclaw-node",
		DeviceIdentityPath: t.TempDir() + "/device-identity.json",
	})
	var deltas []string
	delivery, err := c.SendVoiceTranscriptStream(context.Background(), VoiceTranscriptEvent{
		Text:       "hello",
		SessionKey: "agent:main:main",
		StreamID:   "stream-1",
		Source:     "bbclaw.adapter",
		NodeID:     "bbclaw-node",
	}, func(evt VoiceTranscriptStreamEvent) {
		if evt.Type == "reply.delta" {
			deltas = append(deltas, evt.Text)
		}
	})
	if err != nil {
		t.Fatalf("SendVoiceTranscriptStream() error = %v", err)
	}
	if delivery.ReplyText != "Hello world!" {
		t.Fatalf("reply = %q", delivery.ReplyText)
	}
	want := []string{"Hello", "Hello world", "Hello world!"}
	if len(deltas) != len(want) {
		t.Fatalf("deltas = %#v", deltas)
	}
	for i, got := range deltas {
		if got != want[i] {
			t.Fatalf("delta[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestSendVoiceTranscriptStreamWS_CapturesAgentAssistantStream(t *testing.T) {
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.WriteJSON(map[string]any{
			"type": "event", "event": "connect.challenge",
			"payload": map[string]any{"nonce": "n-1"},
		})

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			reqID, _ := req["id"].(string)
			_ = conn.WriteJSON(map[string]any{"type": "res", "id": reqID, "ok": true})
			if method, _ := req["method"].(string); method == "node.event" {
				if params, ok := req["params"].(map[string]any); ok && params["event"] == "voice.transcript" {
					break
				}
			}
		}

		sessionID := "f8a628e551c04d93be971b2e7be9b7f0"
		events := []map[string]any{
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]any{
					"sessionKey": "agent:main:" + sessionID,
					"stream":     "lifecycle",
					"data":       map[string]any{"phase": "start"},
				},
			},
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]any{
					"sessionKey": "agent:main:" + sessionID,
					"stream":     "assistant",
					"data":       map[string]any{"delta": "Hi!", "text": "Hi!"},
				},
			},
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]any{
					"sessionKey": "agent:main:" + sessionID,
					"stream":     "assistant",
					"data": map[string]any{
						"delta": " 有什么需要帮忙的吗？🦐",
						"text":  "Hi! 有什么需要帮忙的吗？🦐",
					},
				},
			},
			{
				"type":  "event",
				"event": "agent",
				"payload": map[string]any{
					"sessionKey": "agent:main:" + sessionID,
					"stream":     "lifecycle",
					"data":       map[string]any{"phase": "end"},
				},
			},
		}
		for _, evt := range events {
			if err := conn.WriteJSON(evt); err != nil {
				t.Fatalf("write agent event: %v", err)
			}
		}
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	u.Scheme = "ws"

	c := NewClient(u.String(), 500*time.Millisecond, Options{
		NodeID:             "bbclaw-node",
		DeviceIdentityPath: t.TempDir() + "/device-identity.json",
	})
	var deltas []string
	delivery, err := c.SendVoiceTranscriptStream(context.Background(), VoiceTranscriptEvent{
		Text:       "hello",
		SessionKey: "f8a628e551c04d93be971b2e7be9b7f0",
		StreamID:   "stream-1",
		Source:     "bbclaw.adapter",
		NodeID:     "bbclaw-node",
	}, func(evt VoiceTranscriptStreamEvent) {
		if evt.Type == "reply.delta" {
			deltas = append(deltas, evt.Text)
		}
	})
	if err != nil {
		t.Fatalf("SendVoiceTranscriptStream() error = %v", err)
	}
	if delivery.ReplyText != "Hi! 有什么需要帮忙的吗？🦐" {
		t.Fatalf("reply = %q", delivery.ReplyText)
	}
	want := []string{"Hi!", "Hi! 有什么需要帮忙的吗？🦐"}
	if len(deltas) != len(want) {
		t.Fatalf("deltas = %#v", deltas)
	}
	for i, got := range deltas {
		if got != want[i] {
			t.Fatalf("delta[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestSendVoiceTranscriptHTTP_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		params := payload["params"].(map[string]any)
		if params["event"] != "voice.transcript" {
			t.Fatalf("event = %v", params["event"])
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"bbclaw-adapter","result":{"ok":true}}`))
	}))
	defer ts.Close()

	c := NewClient(ts.URL, 2*time.Second)
	delivery, err := c.SendVoiceTranscript(context.Background(), VoiceTranscriptEvent{
		Text:       "hello",
		SessionKey: "agent:main:main",
		StreamID:   "stream-1",
		Source:     "bbclaw.adapter",
		NodeID:     "bbclaw-node",
	})
	if err != nil {
		t.Fatalf("SendVoiceTranscript() error = %v", err)
	}
	if delivery.ReplyText != "" {
		t.Fatalf("reply = %q", delivery.ReplyText)
	}
}

func TestSendVoiceTranscriptHTTP_RPCError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"bbclaw-adapter","error":{"code":-32000,"message":"boom"}}`))
	}))
	defer ts.Close()

	c := NewClient(ts.URL, 2*time.Second)
	_, err := c.SendVoiceTranscript(context.Background(), VoiceTranscriptEvent{
		Text:       "hello",
		SessionKey: "agent:main:main",
		StreamID:   "stream-1",
		Source:     "bbclaw.adapter",
		NodeID:     "bbclaw-node",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// wsTestHandler returns a generic WS handler that responds OK to any request.
// If sendFinalChat is true, it sends a chat final event after seeing voice.transcript.
func wsTestHandler(t *testing.T, sendFinalChat bool) http.HandlerFunc {
	t.Helper()
	upgrader := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return // operator connection may race; silently ignore
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type": "event", "event": "connect.challenge",
			"payload": map[string]any{"nonce": "n-1"},
		})
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			reqID, _ := req["id"].(string)
			_ = conn.WriteJSON(map[string]any{"type": "res", "id": reqID, "ok": true})
			method, _ := req["method"].(string)
			if method == "node.event" {
				if params, ok := req["params"].(map[string]any); ok && params["event"] == "voice.transcript" {
					if sendFinalChat {
						_ = conn.WriteJSON(map[string]any{
							"type": "event", "event": "chat",
							"payload": map[string]any{
								"sessionKey": "agent:main:main", "state": "final",
								"message": map[string]any{
									"role":    "assistant",
									"content": []map[string]any{{"type": "text", "text": "你好，今天是星期六。"}},
								},
							},
						})
					}
					return
				}
			}
		}
	}
}

func TestSendVoiceTranscriptWS_Success(t *testing.T) {
	ts := httptest.NewServer(wsTestHandler(t, false))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	c := NewClient(u.String(), 200*time.Millisecond, Options{
		NodeID: "bbclaw-node", DeviceIdentityPath: t.TempDir() + "/device-identity.json",
	})
	delivery, err := c.SendVoiceTranscript(context.Background(), VoiceTranscriptEvent{
		Text: "hello", SessionKey: "agent:main:main", StreamID: "stream-1",
		Source: "bbclaw.adapter", NodeID: "bbclaw-node",
	})
	if err != nil {
		t.Fatalf("SendVoiceTranscript() error = %v", err)
	}
	if delivery.ReplyText != "" {
		t.Fatalf("reply = %q", delivery.ReplyText)
	}
}

func TestSendVoiceTranscriptWS_CaptureFinalReply(t *testing.T) {
	ts := httptest.NewServer(wsTestHandler(t, true))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	c := NewClient(u.String(), 2*time.Second, Options{
		NodeID: "bbclaw-node", DeviceIdentityPath: t.TempDir() + "/device-identity.json",
	})
	delivery, err := c.SendVoiceTranscript(context.Background(), VoiceTranscriptEvent{
		Text: "hello", SessionKey: "agent:main:main", StreamID: "stream-1",
		Source: "bbclaw.adapter", NodeID: "bbclaw-node",
	})
	if err != nil {
		t.Fatalf("SendVoiceTranscript() error = %v", err)
	}
	if delivery.ReplyText != "你好，今天是星期六。" {
		t.Fatalf("reply = %q", delivery.ReplyText)
	}
}
