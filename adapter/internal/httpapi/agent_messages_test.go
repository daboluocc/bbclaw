package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// mockMessageLoaderDriver implements both Driver and MessageLoader.
type mockMessageLoaderDriver struct {
	name string
	all  []agent.Message
}

func (m *mockMessageLoaderDriver) Name() string                    { return m.name }
func (m *mockMessageLoaderDriver) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (m *mockMessageLoaderDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	return "", nil
}
func (m *mockMessageLoaderDriver) Send(sid agent.SessionID, text string) error { return nil }
func (m *mockMessageLoaderDriver) Events(sid agent.SessionID) <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (m *mockMessageLoaderDriver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return nil
}
func (m *mockMessageLoaderDriver) Stop(sid agent.SessionID) error { return nil }

// LoadMessages mirrors the contract documented on agent.MessageLoader: chronological
// slice ending at `before`, sized at most `limit`. before <= 0 means "latest".
func (m *mockMessageLoaderDriver) LoadMessages(ctx context.Context, sid string, before, limit int) (agent.MessagesPage, error) {
	total := len(m.all)
	end := total
	if before > 0 && before < total {
		end = before
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return agent.MessagesPage{
		Messages: m.all[start:end],
		Total:    total,
		HasMore:  start > 0,
	}, nil
}

func makeSeqMessages(n int) []agent.Message {
	out := make([]agent.Message, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = agent.Message{Role: role, Content: "msg-" + itoa(i), Seq: i}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func decodePage(t *testing.T, body []byte) (response, []agent.Message, int, bool) {
	t.Helper()
	var resp response
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data, _ := resp.Data.(map[string]any)
	if data == nil {
		return resp, nil, 0, false
	}
	rawMsgs, _ := json.Marshal(data["messages"])
	var msgs []agent.Message
	_ = json.Unmarshal(rawMsgs, &msgs)
	totalF, _ := data["total"].(float64)
	hasMore, _ := data["hasMore"].(bool)
	return resp, msgs, int(totalF), hasMore
}

func TestHandleAgentSessionMessages(t *testing.T) {
	log := obs.NewLogger()
	metrics := obs.NewMetrics()

	t.Run("latest page returns last N in chronological order", func(t *testing.T) {
		router := agent.NewRouter()
		drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(10)}
		router.Register(drv, log)

		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=claude-code&limit=4", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
		}
		_, msgs, total, hasMore := decodePage(t, w.Body.Bytes())
		if total != 10 {
			t.Errorf("expected total=10, got %d", total)
		}
		if !hasMore {
			t.Errorf("expected hasMore=true (4/10 returned)")
		}
		if len(msgs) != 4 {
			t.Fatalf("expected 4 messages, got %d", len(msgs))
		}
		if msgs[0].Seq != 6 || msgs[3].Seq != 9 {
			t.Errorf("expected seq [6..9], got [%d..%d]", msgs[0].Seq, msgs[3].Seq)
		}
	})

	t.Run("before cursor pages backward", func(t *testing.T) {
		router := agent.NewRouter()
		drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(10)}
		router.Register(drv, log)

		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		// before=6, limit=4 → window [2..5]; seq 0,1 still exist so hasMore=true.
		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=claude-code&before=6&limit=4", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		_, msgs, _, hasMore := decodePage(t, w.Body.Bytes())
		if !hasMore {
			t.Errorf("expected hasMore=true (seq 0,1 not yet returned)")
		}
		if len(msgs) != 4 {
			t.Fatalf("expected 4 messages, got %d", len(msgs))
		}
		if msgs[0].Seq != 2 || msgs[3].Seq != 5 {
			t.Errorf("expected seq [2..5], got [%d..%d]", msgs[0].Seq, msgs[3].Seq)
		}

		// before=4, limit=10 → window clamps to [0..3]; hasMore=false.
		req2 := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=claude-code&before=4&limit=10", nil)
		req2.SetPathValue("id", "sid-1")
		w2 := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w2, req2)
		_, msgs2, _, hasMore2 := decodePage(t, w2.Body.Bytes())
		if hasMore2 {
			t.Errorf("expected hasMore=false when window starts at 0")
		}
		if len(msgs2) != 4 || msgs2[0].Seq != 0 || msgs2[3].Seq != 3 {
			t.Errorf("expected seq [0..3], got %+v", msgs2)
		}
	})

	t.Run("driver missing MessageLoader returns MESSAGES_NOT_SUPPORTED", func(t *testing.T) {
		router := agent.NewRouter()
		router.Register(&mockBasicDriver{name: "ollama"}, log)

		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=ollama", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 (graceful degrade), got %d", w.Code)
		}
		var resp response
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.OK || resp.Error != "MESSAGES_NOT_SUPPORTED" {
			t.Errorf("expected MESSAGES_NOT_SUPPORTED, got ok=%v error=%s", resp.OK, resp.Error)
		}
	})

	t.Run("unknown driver returns 400", func(t *testing.T) {
		router := agent.NewRouter()
		router.Register(&mockBasicDriver{name: "claude-code"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=nope", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("missing session id returns 400", func(t *testing.T) {
		router := agent.NewRouter()
		router.Register(&mockMessageLoaderDriver{name: "claude-code"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions//messages?driver=claude-code", nil)
		req.SetPathValue("id", "")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("missing driver param returns 400", func(t *testing.T) {
		router := agent.NewRouter()
		router.Register(&mockMessageLoaderDriver{name: "claude-code"}, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("limit clamps to maxMessagesPerPage", func(t *testing.T) {
		router := agent.NewRouter()
		drv := &mockMessageLoaderDriver{name: "claude-code", all: makeSeqMessages(500)}
		router.Register(drv, log)
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		srv.SetAgentRouter(router)

		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=claude-code&limit=10000", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		_, msgs, _, _ := decodePage(t, w.Body.Bytes())
		if len(msgs) != maxMessagesPerPage {
			t.Errorf("expected len=%d (clamp), got %d", maxMessagesPerPage, len(msgs))
		}
	})

	t.Run("router not configured returns 501", func(t *testing.T) {
		srv := NewServer(AppConfig{}, nil, nil, nil, nil, log, metrics)
		req := httptest.NewRequest("GET", "/v1/agent/sessions/sid-1/messages?driver=claude-code", nil)
		req.SetPathValue("id", "sid-1")
		w := httptest.NewRecorder()
		srv.handleAgentSessionMessages(w, req)

		if w.Code != http.StatusNotImplemented {
			t.Errorf("expected 501, got %d", w.Code)
		}
	})
}
