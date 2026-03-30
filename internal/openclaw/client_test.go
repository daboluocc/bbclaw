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
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]any{
				"nonce": "n-1",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		for i := 0; i < 3; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read req %d: %v", i, err)
			}
			var req map[string]any
			if err := json.Unmarshal(msg, &req); err != nil {
				t.Fatalf("decode req %d: %v", i, err)
			}
			reqID, _ := req["id"].(string)
			if err := conn.WriteJSON(map[string]any{
				"type": "res",
				"id":   reqID,
				"ok":   true,
			}); err != nil {
				t.Fatalf("write res %d: %v", i, err)
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

func TestSendVoiceTranscriptWS_Success(t *testing.T) {
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]any{
				"nonce": "n-1",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		_, m1, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read connect req: %v", err)
		}
		var connectReq map[string]any
		if err := json.Unmarshal(m1, &connectReq); err != nil {
			t.Fatalf("decode connect req: %v", err)
		}
		if connectReq["method"] != "connect" {
			t.Fatalf("connect method = %v", connectReq["method"])
		}
		connectID, _ := connectReq["id"].(string)
		if err := conn.WriteJSON(map[string]any{
			"type":    "res",
			"id":      connectID,
			"ok":      true,
			"payload": map[string]any{"type": "hello-ok", "protocol": 3},
		}); err != nil {
			t.Fatalf("write connect res: %v", err)
		}

		_, m2, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read chat.subscribe req: %v", err)
		}
		var subscribeReq map[string]any
		if err := json.Unmarshal(m2, &subscribeReq); err != nil {
			t.Fatalf("decode chat.subscribe req: %v", err)
		}
		if subscribeReq["method"] != "node.event" {
			t.Fatalf("chat.subscribe method = %v", subscribeReq["method"])
		}
		subscribeParams := subscribeReq["params"].(map[string]any)
		if subscribeParams["event"] != "chat.subscribe" {
			t.Fatalf("subscribe event = %v", subscribeParams["event"])
		}
		subscribeID, _ := subscribeReq["id"].(string)
		if err := conn.WriteJSON(map[string]any{
			"type":    "res",
			"id":      subscribeID,
			"ok":      true,
			"payload": map[string]any{"ok": true},
		}); err != nil {
			t.Fatalf("write subscribe res: %v", err)
		}

		_, m3, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read node.event req: %v", err)
		}
		var nodeEventReq map[string]any
		if err := json.Unmarshal(m3, &nodeEventReq); err != nil {
			t.Fatalf("decode node.event req: %v", err)
		}
		if nodeEventReq["method"] != "node.event" {
			t.Fatalf("node.event method = %v", nodeEventReq["method"])
		}
		params := nodeEventReq["params"].(map[string]any)
		if params["event"] != "voice.transcript" {
			t.Fatalf("event = %v", params["event"])
		}
		nodeEventID, _ := nodeEventReq["id"].(string)
		if err := conn.WriteJSON(map[string]any{
			"type":    "res",
			"id":      nodeEventID,
			"ok":      true,
			"payload": map[string]any{"ok": true},
		}); err != nil {
			t.Fatalf("write node.event res: %v", err)
		}
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	u.Scheme = "ws"

	c := NewClient(u.String(), 200*time.Millisecond, Options{
		NodeID:             "bbclaw-node",
		DeviceIdentityPath: t.TempDir() + "/device-identity.json",
	})
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

func TestSendVoiceTranscriptWS_CaptureFinalReply(t *testing.T) {
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{
			"type":  "event",
			"event": "connect.challenge",
			"payload": map[string]any{
				"nonce": "n-1",
			},
		}); err != nil {
			t.Fatalf("write challenge: %v", err)
		}

		_, m1, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read connect req: %v", err)
		}
		var connectReq map[string]any
		if err := json.Unmarshal(m1, &connectReq); err != nil {
			t.Fatalf("decode connect req: %v", err)
		}
		connectID, _ := connectReq["id"].(string)
		if err := conn.WriteJSON(map[string]any{
			"type": "res",
			"id":   connectID,
			"ok":   true,
		}); err != nil {
			t.Fatalf("write connect res: %v", err)
		}

		_, m2, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read subscribe req: %v", err)
		}
		var subscribeReq map[string]any
		if err := json.Unmarshal(m2, &subscribeReq); err != nil {
			t.Fatalf("decode subscribe req: %v", err)
		}
		subscribeID, _ := subscribeReq["id"].(string)
		if err := conn.WriteJSON(map[string]any{
			"type": "res",
			"id":   subscribeID,
			"ok":   true,
		}); err != nil {
			t.Fatalf("write subscribe res: %v", err)
		}

		_, m3, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read node.event req: %v", err)
		}
		var nodeEventReq map[string]any
		if err := json.Unmarshal(m3, &nodeEventReq); err != nil {
			t.Fatalf("decode node.event req: %v", err)
		}
		nodeEventID, _ := nodeEventReq["id"].(string)
		if err := conn.WriteJSON(map[string]any{
			"type": "res",
			"id":   nodeEventID,
			"ok":   true,
		}); err != nil {
			t.Fatalf("write node.event res: %v", err)
		}

		if err := conn.WriteJSON(map[string]any{
			"type":  "event",
			"event": "chat",
			"payload": map[string]any{
				"sessionKey": "agent:main:main",
				"state":      "final",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "text", "text": "你好，今天是星期六。"},
					},
				},
			},
		}); err != nil {
			t.Fatalf("write chat event: %v", err)
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
	if delivery.ReplyText != "你好，今天是星期六。" {
		t.Fatalf("reply = %q", delivery.ReplyText)
	}
}
