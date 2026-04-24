package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// fakeOllama is an httptest.Server hook that records each /api/chat request
// and replies with a canned NDJSON stream per call.
type fakeOllama struct {
	mu       sync.Mutex
	requests []ollamaChatRequest
	// replies holds one NDJSON transcript per call. If the test issues more
	// calls than len(replies), the last transcript is reused.
	replies [][]string
	calls   int
}

func newFakeOllama(replies ...[]string) *fakeOllama {
	return &fakeOllama{replies: replies}
}

func (f *fakeOllama) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req ollamaChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		f.requests = append(f.requests, req)
		idx := f.calls
		if idx >= len(f.replies) {
			idx = len(f.replies) - 1
		}
		lines := f.replies[idx]
		f.calls++
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, l := range lines {
			_, _ = w.Write([]byte(l))
			if !strings.HasSuffix(l, "\n") {
				_, _ = w.Write([]byte("\n"))
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (f *fakeOllama) snapshot() []ollamaChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ollamaChatRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func drainAll(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	var out []agent.Event
	tm := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Type == agent.EvTurnEnd {
				return out
			}
		case <-tm:
			t.Fatalf("timed out waiting for TurnEnd; collected=%+v", out)
		}
	}
}

// canned streams: a text chunk, a second text chunk, and a done frame with
// token counts.
func cannedStream(reply string, promptCount, evalCount int) []string {
	return []string{
		`{"model":"m","message":{"role":"assistant","content":"` + reply[:len(reply)/2] + `"},"done":false}`,
		`{"model":"m","message":{"role":"assistant","content":"` + reply[len(reply)/2:] + `"},"done":false}`,
		`{"model":"m","done":true,"total_duration":1000,"eval_count":` + itoa(evalCount) + `,"prompt_eval_count":` + itoa(promptCount) + `}`,
	}
}

func itoa(i int) string {
	return strings.TrimSpace((func() string {
		b, _ := json.Marshal(i)
		return string(b)
	})())
}

func TestDriver_NameAndCaps(t *testing.T) {
	d := New(Options{BaseURL: "http://x"}, obs.NewLogger())
	if d.Name() != "ollama" {
		t.Errorf("Name=%s want ollama", d.Name())
	}
	caps := d.Capabilities()
	if caps.ToolApproval {
		t.Error("ToolApproval must be false")
	}
	if !caps.Resume || !caps.Streaming {
		t.Error("Resume and Streaming must be true")
	}
	if caps.MaxInputBytes != 64*1024 {
		t.Errorf("MaxInputBytes=%d want %d", caps.MaxInputBytes, 64*1024)
	}
}

func TestDriver_SendEmitsEvents(t *testing.T) {
	fake := newFakeOllama(cannedStream("Hi there, friend.", 11, 5))
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d := New(Options{BaseURL: ts.URL, Model: "testmodel", HTTPClient: ts.Client()}, obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := d.Events(sid)

	done := make(chan error, 1)
	go func() { done <- d.Send(sid, "hello there") }()

	evs := drainAll(t, events, 3*time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Send err: %v", err)
	}

	// Expect: 2 text events, 1 tokens event, 1 turn_end.
	var texts []string
	var sawTokens bool
	var sawTurnEnd bool
	for _, e := range evs {
		switch e.Type {
		case agent.EvText:
			texts = append(texts, e.Text)
		case agent.EvTokens:
			sawTokens = true
			if e.Tokens == nil || e.Tokens.In != 11 || e.Tokens.Out != 5 {
				t.Errorf("tokens event wrong: %+v", e.Tokens)
			}
		case agent.EvTurnEnd:
			sawTurnEnd = true
		}
	}
	if len(texts) != 2 {
		t.Errorf("want 2 text events, got %d: %v", len(texts), texts)
	}
	full := strings.Join(texts, "")
	if full != "Hi there, friend." {
		t.Errorf("assembled reply=%q want %q", full, "Hi there, friend.")
	}
	if !sawTokens {
		t.Error("missing EvTokens")
	}
	if !sawTurnEnd {
		t.Error("missing EvTurnEnd")
	}

	// Verify the outbound request body.
	reqs := fake.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("fake recv %d requests, want 1", len(reqs))
	}
	if reqs[0].Model != "testmodel" {
		t.Errorf("req.Model=%s want testmodel", reqs[0].Model)
	}
	if !reqs[0].Stream {
		t.Errorf("req.Stream=false want true")
	}
	if len(reqs[0].Messages) != 1 || reqs[0].Messages[0].Role != "user" || reqs[0].Messages[0].Content != "hello there" {
		t.Errorf("req.Messages=%+v", reqs[0].Messages)
	}
}

func TestDriver_MultiSendAccumulatesHistory(t *testing.T) {
	fake := newFakeOllama(
		cannedStream("Hi!", 5, 2),
		cannedStream("Again.", 7, 3),
	)
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	d := New(Options{BaseURL: ts.URL, HTTPClient: ts.Client()}, obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := d.Events(sid)

	// Turn 1
	done := make(chan error, 1)
	go func() { done <- d.Send(sid, "first") }()
	drainAll(t, events, 3*time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Send 1: %v", err)
	}

	// Turn 2
	go func() { done <- d.Send(sid, "second") }()
	drainAll(t, events, 3*time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	reqs := fake.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	// Turn 2 request must carry: user1, assistant1, user2.
	want := []ollamaMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "Hi!"},
		{Role: "user", Content: "second"},
	}
	if len(reqs[1].Messages) != len(want) {
		t.Fatalf("turn 2 msg count=%d want=%d: %+v", len(reqs[1].Messages), len(want), reqs[1].Messages)
	}
	for i, m := range want {
		if reqs[1].Messages[i] != m {
			t.Errorf("turn 2 msg[%d]=%+v want %+v", i, reqs[1].Messages[i], m)
		}
	}
}

func TestDriver_HistoryCap(t *testing.T) {
	// Force 60 user turns; each returns an empty-ish assistant reply. The
	// driver should cap messages at maxHistoryPairs (50) and drop oldest
	// entries.
	d := &Driver{
		baseURL:  "",
		model:    "m",
		log:      obs.NewLogger(),
		sessions: make(map[agent.SessionID]*session),
	}
	sid, _ := d.Start(context.Background(), agent.StartOpts{})
	d.mu.Lock()
	s := d.sessions[sid]
	d.mu.Unlock()

	for i := 0; i < 60; i++ {
		s.appendMessage(ollamaMessage{Role: "user", Content: "u"})
		s.appendMessage(ollamaMessage{Role: "assistant", Content: "a"})
	}
	snap := s.snapshotMessages()
	if len(snap) != maxHistoryPairs {
		t.Fatalf("history len=%d want %d", len(snap), maxHistoryPairs)
	}
}

func TestDriver_HistoryCapPreservesSystem(t *testing.T) {
	d := &Driver{
		baseURL:  "",
		model:    "m",
		log:      obs.NewLogger(),
		sessions: make(map[agent.SessionID]*session),
	}
	sid, _ := d.Start(context.Background(), agent.StartOpts{})
	d.mu.Lock()
	s := d.sessions[sid]
	d.mu.Unlock()

	// Inject a system message first, then overflow.
	s.appendMessage(ollamaMessage{Role: "system", Content: "be brief"})
	for i := 0; i < 80; i++ {
		s.appendMessage(ollamaMessage{Role: "user", Content: "u"})
	}
	snap := s.snapshotMessages()
	if len(snap) > maxHistoryPairs+1 {
		t.Fatalf("history len=%d want <= %d (system preserved)", len(snap), maxHistoryPairs+1)
	}
	if snap[0].Role != "system" || snap[0].Content != "be brief" {
		t.Errorf("system message lost: head=%+v", snap[0])
	}
}

// Integration test: hits a real Ollama server on 127.0.0.1:11434. Skipped
// unless BBCLAW_OLLAMA_INTEGRATION=1 is set.
func TestDriver_Integration(t *testing.T) {
	if os.Getenv("BBCLAW_OLLAMA_INTEGRATION") != "1" {
		t.Skip("set BBCLAW_OLLAMA_INTEGRATION=1 to run (requires ollama on 11434)")
	}
	d := New(Options{}, obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := d.Events(sid)
	done := make(chan error, 1)
	go func() { done <- d.Send(sid, "Say hi in exactly 3 words.") }()
	evs := drainAll(t, events, 60*time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	var fullText strings.Builder
	for _, e := range evs {
		if e.Type == agent.EvText {
			fullText.WriteString(e.Text)
		}
	}
	if fullText.Len() == 0 {
		t.Error("expected some text")
	}
	t.Logf("assistant reply: %q", fullText.String())
}
