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
//
// It supports multi-turn reuse: each Send pushes a full 4-event reply
// (text, text, tokens, turn_end) onto the shared events channel. Stop
// closes the channel once; repeat calls are idempotent.
type mockDriver struct {
	mu       sync.Mutex
	events   chan agent.Event
	received []string
	stopped  bool
}

func newMockDriver() *mockDriver {
	return &mockDriver{events: make(chan agent.Event, 64)}
}

func (m *mockDriver) Name() string                    { return "mock" }
func (m *mockDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Streaming: true} }

func (m *mockDriver) Start(_ context.Context, _ agent.StartOpts) (agent.SessionID, error) {
	return "mock-sid", nil
}

func (m *mockDriver) Send(_ agent.SessionID, text string) error {
	m.mu.Lock()
	m.received = append(m.received, text)
	m.mu.Unlock()
	// Simulate a streamed reply. Events are pushed synchronously so
	// successive Send calls produce deterministic ordering.
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

// receivedTexts returns a snapshot of every text Send has been called with.
func (m *mockDriver) receivedTexts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.received))
	copy(out, m.received)
	return out
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

	frames := readNDJSONFrames(t, resp.Body)

	if got := mock.receivedTexts(); len(got) != 1 || got[0] != "hi there" {
		t.Errorf("driver received=%v, want ['hi there']", got)
	}

	// Expected: session frame + 2 text + tokens + turn_end = 5.
	if len(frames) != 5 {
		t.Fatalf("want 5 frames (session + 2 text + tokens + turn_end), got %d: %+v", len(frames), frames)
	}
	if frames[0]["type"] != "session" {
		t.Errorf("frame 0 type=%v, want session: %+v", frames[0]["type"], frames[0])
	}
	if frames[0]["isNew"] != true {
		t.Errorf("frame 0 isNew=%v, want true", frames[0]["isNew"])
	}
	if sid, ok := frames[0]["sessionId"].(string); !ok || sid == "" {
		t.Errorf("frame 0 missing sessionId: %+v", frames[0])
	}
	if frames[1]["type"] != "text" || frames[1]["text"] != "hello " {
		t.Errorf("frame 1: %+v", frames[1])
	}
	if frames[2]["type"] != "text" || frames[2]["text"] != "from mock" {
		t.Errorf("frame 2: %+v", frames[2])
	}
	if frames[3]["type"] != "tokens" || frames[3]["in"] != float64(7) || frames[3]["out"] != float64(3) {
		t.Errorf("frame 3: %+v", frames[3])
	}
	if frames[4]["type"] != "turn_end" {
		t.Errorf("frame 4: %+v", frames[4])
	}
}

func TestHandleAgentMessage_MultiTurn(t *testing.T) {
	mock := newMockDriver()
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(mock)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// First turn: no sessionId supplied, expect isNew=true.
	resp1, err := http.Post(ts.URL+"/v1/agent/message", "application/json",
		bytes.NewBufferString(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("POST turn 1: %v", err)
	}
	frames1 := readNDJSONFrames(t, resp1.Body)
	resp1.Body.Close()

	if len(frames1) == 0 || frames1[0]["type"] != "session" {
		t.Fatalf("turn 1: first frame must be session, got %+v", frames1)
	}
	if frames1[0]["isNew"] != true {
		t.Errorf("turn 1 isNew=%v, want true", frames1[0]["isNew"])
	}
	sid1, ok := frames1[0]["sessionId"].(string)
	if !ok || sid1 == "" {
		t.Fatalf("turn 1: missing sessionId: %+v", frames1[0])
	}

	// Second turn: pass the sid back, expect isNew=false and the same sid.
	body2, _ := json.Marshal(map[string]any{"text": "bye", "sessionId": sid1})
	resp2, err := http.Post(ts.URL+"/v1/agent/message", "application/json",
		bytes.NewReader(body2))
	if err != nil {
		t.Fatalf("POST turn 2: %v", err)
	}
	frames2 := readNDJSONFrames(t, resp2.Body)
	resp2.Body.Close()

	if len(frames2) == 0 || frames2[0]["type"] != "session" {
		t.Fatalf("turn 2: first frame must be session, got %+v", frames2)
	}
	if frames2[0]["isNew"] != false {
		t.Errorf("turn 2 isNew=%v, want false", frames2[0]["isNew"])
	}
	if sid2, _ := frames2[0]["sessionId"].(string); sid2 != sid1 {
		t.Errorf("turn 2 sessionId=%q, want %q", sid2, sid1)
	}

	// Both Send invocations should be recorded in order.
	got := mock.receivedTexts()
	want := []string{"hi", "bye"}
	if len(got) != len(want) {
		t.Fatalf("driver received=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("driver received[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// readNDJSONFrames parses a newline-delimited JSON stream into one map per
// line. Fails the test on any parse error.
func readNDJSONFrames(t *testing.T, r interface {
	Read(p []byte) (n int, err error)
}) []map[string]any {
	t.Helper()
	var frames []map[string]any
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
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
	return frames
}
