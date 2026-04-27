package aider

import (
	"context"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func TestParseStdoutBasic(t *testing.T) {
	const transcript = `Aider v0.65.0
Model: claude-3-5-sonnet
Git repo: none
Repo-map: disabled

Hello there! I can help you with that.

Here is the answer to your question.
Tokens: 42 sent, 18 received.
Cost: $0.0003 message, $0.0003 session.
`
	s := &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 32),
		rootCtx: context.Background(),
	}

	parseStdout(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []string
	for e := range s.events {
		if e.Type != agent.EvText {
			t.Errorf("unexpected non-text event: %+v", e)
			continue
		}
		got = append(got, e.Text)
	}

	joined := strings.Join(got, "")
	if !strings.Contains(joined, "Hello there!") {
		t.Errorf("want 'Hello there!' in output, got %q", joined)
	}
	if !strings.Contains(joined, "Here is the answer") {
		t.Errorf("want 'Here is the answer' in output, got %q", joined)
	}
	if strings.Contains(joined, "Aider v") {
		t.Errorf("banner 'Aider v' should be filtered, got %q", joined)
	}
	if strings.Contains(joined, "Tokens:") {
		t.Errorf("'Tokens:' line should be filtered, got %q", joined)
	}
	if strings.Contains(joined, "Cost:") {
		t.Errorf("'Cost:' line should be filtered, got %q", joined)
	}
	if strings.Contains(joined, "Git repo:") {
		t.Errorf("'Git repo:' should be filtered, got %q", joined)
	}
}

func TestShouldFilterLine(t *testing.T) {
	cases := []struct {
		line string
		drop bool
	}{
		{"Aider v0.65.0", true},
		{"Model: claude-3-5-sonnet", true},
		{"Git repo: /tmp/aider", true},
		{"Repo-map: disabled", true},
		{"Use /help <question> for help", true},
		{"Tokens: 42 sent", true},
		{"Cost: $0.001", true},
		{"> tell me a joke", true},
		{"────────────", true},
		{"━━━━━━━━━━━━", true},
		{"Hello world", false},
		{"This is the actual answer.", false},
		{"  ", false}, // empty/whitespace forwarded
		{"", false},
	}
	for _, c := range cases {
		got := shouldFilterLine(c.line)
		if got != c.drop {
			t.Errorf("shouldFilterLine(%q): got %v, want %v", c.line, got, c.drop)
		}
	}
}

func TestStartAndStop(t *testing.T) {
	d := New(Options{Bin: "/nonexistent/aider"}, obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(string(sid), "ai-") {
		t.Errorf("session id prefix: want 'ai-', got %q", sid)
	}
	d.mu.Lock()
	s := d.sessions[sid]
	d.mu.Unlock()
	if s == nil {
		t.Fatal("session not registered")
	}
	if s.historyPath == "" {
		t.Error("historyPath should be allocated")
	}
	if !strings.Contains(s.historyPath, string(sid)) {
		t.Errorf("historyPath should contain sid, got %q", s.historyPath)
	}

	if err := d.Stop(sid); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := d.Stop(sid); err != agent.ErrUnknownSession {
		t.Errorf("Stop after stop: want ErrUnknownSession, got %v", err)
	}
}

func TestCapabilities(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	caps := d.Capabilities()
	if caps.ToolApproval {
		t.Error("aider does not support tool approval (auto-yes)")
	}
	if !caps.Resume {
		t.Error("aider supports resume via chat-history-file")
	}
	if caps.MaxInputBytes <= 0 {
		t.Error("MaxInputBytes should be positive")
	}
}

func TestApproveUnsupported(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	sid, _ := d.Start(context.Background(), agent.StartOpts{})
	defer d.Stop(sid)
	err := d.Approve(sid, "tid-1", agent.DecisionOnce)
	if err != agent.ErrUnsupported {
		t.Errorf("Approve: want ErrUnsupported, got %v", err)
	}
}

func TestEventsUnknownSession(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	ch := d.Events("nope")
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Events for unknown session should return closed channel")
		}
	default:
		t.Error("Events for unknown session should return immediately-closed channel")
	}
}
