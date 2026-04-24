package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// mockDriver is a deterministic agent.Driver used to verify the HTTP
// handler's event plumbing without spawning any subprocess.
type mockDriver struct {
	mu       sync.Mutex
	events   chan agent.Event
	received string
	stopped  bool
}

func newMockDriver() *mockDriver {
	return &mockDriver{events: make(chan agent.Event, 16)}
}

func (m *mockDriver) Name() string                   { return "mock" }
func (m *mockDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Streaming: true} }

func (m *mockDriver) Start(_ context.Context, _ agent.StartOpts) (agent.SessionID, error) {
	return "mock-sid", nil
}

func (m *mockDriver) Send(_ agent.SessionID, text string) error {
	m.mu.Lock()
	m.received = text
	m.mu.Unlock()
	// simulate a streamed reply
	m.events <- agent.Event{Type: agent.EvText, Text: "hello "}
	m.events <- agent.Event{Type: agent.EvText, Text: "from mock"}
	m.events <- agent.Event{Type: agent.EvTokens, Tokens: &agent.Tokens{In: 7, Out: 3}}
	m.events <- agent.Event{Type: agent.EvTurnEnd}
	return nil
}

func (m *mockDriver) Events(_ agent.SessionID) <-chan agent.Event { return m.events }

func (m *mockDriver) Approve(_ agent.SessionID, _ agent.ToolID, _ agent.Decision) error {
	return agent.ErrUnsupported
}

func (m *mockDriver) Stop(_ agent.SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stopped {
		m.stopped = true
		close(m.events)
	}
	return nil
}

func TestHandleAgentMessage_NotConfigured(t *testing.T) {
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/agent/message", "application/json",
		strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status=%d, want 501", resp.StatusCode)
	}
}

func TestHandleAgentMessage_EmptyText(t *testing.T) {
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(newMockDriver())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/agent/message", "application/json",
		strings.NewReader(`{"text":"   "}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestHandleAgentMessage_StreamsEvents(t *testing.T) {
	mock := newMockDriver()
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(mock)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/agent/message", "application/json",
		bytes.NewBufferString(`{"text":"hi there"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("content-type=%q, want application/x-ndjson", ct)
	}

	var frames []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var f map[string]any
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("unparseable frame: %v line=%s", err, sc.Text())
		}
		frames = append(frames, f)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if mock.received != "hi there" {
		t.Errorf("driver received=%q, want 'hi there'", mock.received)
	}

	if len(frames) != 4 {
		t.Fatalf("want 4 frames (2 text + tokens + turn_end), got %d: %+v", len(frames), frames)
	}
	if frames[0]["type"] != "text" || frames[0]["text"] != "hello " {
		t.Errorf("frame 0: %+v", frames[0])
	}
	if frames[1]["type"] != "text" || frames[1]["text"] != "from mock" {
		t.Errorf("frame 1: %+v", frames[1])
	}
	if frames[2]["type"] != "tokens" || frames[2]["in"] != float64(7) || frames[2]["out"] != float64(3) {
		t.Errorf("frame 2: %+v", frames[2])
	}
	if frames[3]["type"] != "turn_end" {
		t.Errorf("frame 3: %+v", frames[3])
	}
}
