package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// Test the stream-json parser by feeding it a canned transcript that
// exercises init / assistant text / tool_use / result.
func TestParseStreamJSON(t *testing.T) {
	const transcript = `{"type":"system","subtype":"init","session_id":"abc-123","model":"claude-sonnet-4-6"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":" world"},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls -la"}}]}}
{"type":"result","subtype":"success","result":"Hello world","usage":{"input_tokens":12,"output_tokens":3}}
`

	s := &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 16),
		rootCtx: context.Background(),
	}

	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 5 {
		t.Fatalf("want 5 events (1 session_init + 2 text + 1 tool_call + 1 tokens), got %d: %+v", len(got), got)
	}

	if got[0].Type != agent.EvSessionInit || got[0].Text != "abc-123" {
		t.Errorf("event 0: want EvSessionInit 'abc-123', got %+v", got[0])
	}
	if got[1].Type != agent.EvText || got[1].Text != "Hello" {
		t.Errorf("event 1: want EvText 'Hello', got %+v", got[1])
	}
	if got[2].Type != agent.EvText || got[2].Text != " world" {
		t.Errorf("event 2: want EvText ' world', got %+v", got[2])
	}
	if got[3].Type != agent.EvToolCall || got[3].Tool == nil {
		t.Fatalf("event 3: want EvToolCall, got %+v", got[3])
	}
	if got[3].Tool.Tool != "Bash" || got[3].Tool.Hint != "ls -la" || got[3].Tool.ID != "tu_1" {
		t.Errorf("tool_call: want tool=Bash hint='ls -la' id=tu_1, got %+v", got[3].Tool)
	}
	if got[4].Type != agent.EvTokens || got[4].Tokens == nil {
		t.Fatalf("event 4: want EvTokens, got %+v", got[4])
	}
	if got[4].Tokens.In != 12 || got[4].Tokens.Out != 3 {
		t.Errorf("tokens: want in=12 out=3, got %+v", got[4].Tokens)
	}

	// Resume ID should have been captured from the init event.
	if s.resumeID != "abc-123" {
		t.Errorf("resumeID: want 'abc-123', got %q", s.resumeID)
	}
}

// TestParseStreamJSONToolUseEmitsEvToolCall feeds a transcript with a single
// tool_use block whose command is longer than the 80-char truncation limit
// and verifies exactly one EvToolCall event is emitted with the expected
// tool name and truncated hint.
func TestParseStreamJSONToolUseEmitsEvToolCall(t *testing.T) {
	longCmd := strings.Repeat("x", 120)
	transcript := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_42","name":"Bash","input":{"command":"` + longCmd + `"}}]}}` + "\n"

	s := &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 4),
		rootCtx: context.Background(),
	}
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var tools []agent.Event
	for e := range s.events {
		if e.Type == agent.EvToolCall {
			tools = append(tools, e)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("want exactly 1 EvToolCall, got %d", len(tools))
	}
	ev := tools[0]
	if ev.Tool == nil {
		t.Fatalf("Tool payload is nil")
	}
	if ev.Tool.Tool != "Bash" {
		t.Errorf("tool name: want Bash, got %q", ev.Tool.Tool)
	}
	if ev.Tool.ID != "tu_42" {
		t.Errorf("tool id: want tu_42, got %q", ev.Tool.ID)
	}
	// Hint must be truncated: 80 chars + the ellipsis suffix.
	wantPrefix := strings.Repeat("x", 80)
	if !strings.HasPrefix(ev.Tool.Hint, wantPrefix) {
		t.Errorf("hint prefix: want 80 x's, got %q", ev.Tool.Hint)
	}
	if !strings.HasSuffix(ev.Tool.Hint, "…") {
		t.Errorf("hint should end with ellipsis on truncation, got %q", ev.Tool.Hint)
	}
}

// TestSummarizeToolInput covers the small switch in summarizeToolInput so
// that changes to the field mapping are caught by a unit test rather than a
// manual playground session.
func TestSummarizeToolInput(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash", "Bash", `{"command":"ls -la /tmp"}`, "ls -la /tmp"},
		{"bash trims whitespace", "Bash", `{"command":"   echo hi   "}`, "echo hi"},
		{"edit file_path", "Edit", `{"file_path":"/a/b.go","old_string":"x"}`, "/a/b.go"},
		{"write file_path", "Write", `{"file_path":"/a/b.go","content":"..."}`, "/a/b.go"},
		{"read file_path", "Read", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"unknown tool returns empty", "Weirdo", `{"x":1}`, ""},
		{"empty raw returns empty", "Bash", ``, ""},
		{"malformed json returns empty", "Bash", `{not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeToolInput(tc.tool, []byte(tc.input))
			if got != tc.want {
				t.Errorf("summarizeToolInput(%s, %s): want %q, got %q", tc.tool, tc.input, tc.want, got)
			}
		})
	}
}

func TestParseStreamJSONMalformedLineIsSkipped(t *testing.T) {
	const transcript = `not-json
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
`
	s := &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 4),
		rootCtx: context.Background(),
	}

	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 1 || got[0].Type != agent.EvText || got[0].Text != "ok" {
		t.Fatalf("want 1 EvText 'ok', got %+v", got)
	}
}

func TestCapabilitiesStable(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	caps := d.Capabilities()
	if caps.ToolApproval {
		t.Error("Phase 1 must not advertise ToolApproval (not yet wired)")
	}
	if !caps.Resume || !caps.Streaming {
		t.Error("Resume and Streaming must be true")
	}
	if caps.MaxInputBytes <= 0 {
		t.Error("MaxInputBytes must be positive")
	}
}

func TestApproveReturnsUnsupported(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	err := d.Approve("any", "t1", agent.DecisionOnce)
	if err != agent.ErrUnsupported {
		t.Errorf("want ErrUnsupported, got %v", err)
	}
}

func TestDriverName(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	if d.Name() != "claude-code" {
		t.Errorf("Name: want 'claude-code', got %q", d.Name())
	}
}

// TestCLISessionExists verifies the filesystem check used by the agent proxy
// to skip doomed --resume attempts. The driver should return false when no
// matching JSONL exists and true when one is present.
func TestCLISessionExists(t *testing.T) {
	d := New(Options{}, obs.NewLogger())

	// Point CLAUDE_SESSIONS_DIR at a temp dir so we don't touch ~/.claude.
	// The driver derives projectsDir as filepath.Join(filepath.Dir(sessionsDir), "projects").
	sessionsDir := t.TempDir()
	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)

	// No projects dir yet → false, no panic.
	if d.CLISessionExists("abc-123") {
		t.Error("CLISessionExists: want false when projects dir is absent")
	}
	if d.CLISessionExists("") {
		t.Error("CLISessionExists: want false for empty id")
	}

	// Create a fake JSONL transcript under projects/<project>/abc-123.jsonl.
	projectsDir := filepath.Join(filepath.Dir(sessionsDir), "projects")
	projectSubdir := filepath.Join(projectsDir, "-tmp-myproject")
	if err := os.MkdirAll(projectSubdir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	jsonlPath := filepath.Join(projectSubdir, "abc-123.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !d.CLISessionExists("abc-123") {
		t.Error("CLISessionExists: want true when JSONL file exists")
	}
	// A different id in the same project dir should still return false.
	if d.CLISessionExists("xyz-999") {
		t.Error("CLISessionExists: want false for id with no matching file")
	}
}
