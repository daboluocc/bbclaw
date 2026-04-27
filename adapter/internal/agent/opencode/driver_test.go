package opencode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func TestParseStream(t *testing.T) {
	const transcript = `{"type":"step_start","sessionID":"ses_abc123","part":{"id":"prt_1","type":"step-start"}}
{"type":"text","sessionID":"ses_abc123","part":{"type":"text","text":"Hello"}}
{"type":"text","sessionID":"ses_abc123","part":{"type":"text","text":" world"}}
{"type":"tool_use","sessionID":"ses_abc123","part":{"type":"tool","tool":"Bash","callID":"tu_1","state":{"status":"completed","input":{"command":"ls -la"}}}}
{"type":"step_finish","sessionID":"ses_abc123","part":{"type":"step-finish","reason":"stop","tokens":{"input":12,"output":3}}}
`

	s := &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 16),
		rootCtx: context.Background(),
	}

	parseStream(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 events (2 text + 1 tool_call + 1 tokens), got %d: %+v", len(got), got)
	}

	if got[0].Type != agent.EvText || got[0].Text != "Hello" {
		t.Errorf("event 0: want EvText 'Hello', got %+v", got[0])
	}
	if got[1].Type != agent.EvText || got[1].Text != " world" {
		t.Errorf("event 1: want EvText ' world', got %+v", got[1])
	}
	if got[2].Type != agent.EvToolCall || got[2].Tool == nil {
		t.Fatalf("event 2: want EvToolCall, got %+v", got[2])
	}
	if got[2].Tool.Tool != "Bash" || got[2].Tool.Hint != "ls -la" || got[2].Tool.ID != "tu_1" {
		t.Errorf("tool_call: want tool=Bash hint='ls -la' id=tu_1, got %+v", got[2].Tool)
	}
	if got[3].Type != agent.EvTokens || got[3].Tokens == nil {
		t.Fatalf("event 3: want EvTokens, got %+v", got[3])
	}
	if got[3].Tokens.In != 12 || got[3].Tokens.Out != 3 {
		t.Errorf("tokens: want in=12 out=3, got %+v", got[3].Tokens)
	}

	if s.resumeID != "ses_abc123" {
		t.Errorf("resumeID: want 'ses_abc123', got %q", s.resumeID)
	}
}

func TestParseStreamMultiStep(t *testing.T) {
	// Two steps in one stream — step_finish with reason="tool-calls"
	// does NOT emit EvTurnEnd; the next step_start continues the stream.
	const transcript = `{"type":"step_start","sessionID":"ses_multi","part":{"id":"prt_1","type":"step-start"}}
{"type":"tool_use","sessionID":"ses_multi","part":{"type":"tool","tool":"Bash","callID":"t1","state":{"status":"completed","input":{"command":"ls"}}}}
{"type":"step_finish","sessionID":"ses_multi","part":{"type":"step-finish","reason":"tool-calls","tokens":{"input":5,"output":2}}}
{"type":"step_start","sessionID":"ses_multi","part":{"id":"prt_2","type":"step-start"}}
{"type":"text","sessionID":"ses_multi","part":{"type":"text","text":"done"}}
{"type":"step_finish","sessionID":"ses_multi","part":{"type":"step-finish","reason":"stop","tokens":{"input":3,"output":1}}}
`

	s := &session{
		id:      "sid-multi",
		events:  make(chan agent.Event, 16),
		rootCtx: context.Background(),
	}

	parseStream(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 events (1 tool + 1 tokens + 1 text + 1 tokens), got %d: %+v", len(got), got)
	}

	toolIdx := findEvent(got, agent.EvToolCall)
	textIdx := findEvent(got, agent.EvText)
	if toolIdx < 0 || textIdx < 0 {
		t.Fatalf("missing tool_call or text event: %+v", got)
	}
	if toolIdx > textIdx {
		t.Errorf("tool event should appear before text event in multi-step stream")
	}

	tokenCount := 0
	for _, e := range got {
		if e.Type == agent.EvTokens {
			tokenCount++
		}
	}
	if tokenCount != 2 {
		t.Errorf("want 2 EvTokens (one per step_finish), got %d", tokenCount)
	}
}

func findEvent(events []agent.Event, typ agent.EventType) int {
	for i, e := range events {
		if e.Type == typ {
			return i
		}
	}
	return -1
}

func TestParseStreamReasoningIsSkipped(t *testing.T) {
	// reasoning events should be logged but NOT emitted as EvText.
	const transcript = `{"type":"step_start","sessionID":"ses_r","part":{"id":"prt_1","type":"step-start"}}
{"type":"reasoning","sessionID":"ses_r","part":{"type":"reasoning","text":"I need to check the list of files"}}
{"type":"text","sessionID":"ses_r","part":{"type":"text","text":"ok"}}
{"type":"step_finish","sessionID":"ses_r","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":1}}}
`

	s := &session{
		id:      "sid-reasoning",
		events:  make(chan agent.Event, 16),
		rootCtx: context.Background(),
	}

	parseStream(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 events (1 text + 1 tokens), got %d: %+v", len(got), got)
	}
	if got[0].Type != agent.EvText || got[0].Text != "ok" {
		t.Errorf("event 0: want EvText 'ok', got %+v", got[0])
	}
}

func TestParseStreamMalformedLineIsSkipped(t *testing.T) {
	const transcript = `not-json
{"type":"step_start","sessionID":"ses_ok","part":{"id":"prt_1","type":"step-start"}}
{"type":"text","sessionID":"ses_ok","part":{"type":"text","text":"ok"}}
{"type":"step_finish","sessionID":"ses_ok","part":{"type":"step-finish","reason":"stop"}}
`

	s := &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 4),
		rootCtx: context.Background(),
	}

	parseStream(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 1 || got[0].Type != agent.EvText || got[0].Text != "ok" {
		t.Fatalf("want 1 EvText 'ok', got %+v", got)
	}

	if s.resumeID != "ses_ok" {
		t.Errorf("resumeID: want 'ses_ok', got %q", s.resumeID)
	}
}

func TestParseStreamResumeIDOnlyFirstStep(t *testing.T) {
	// resumeID should be captured from the first step_start and NOT
	// overwritten by subsequent step_start events.
	const transcript = `{"type":"step_start","sessionID":"ses_first","part":{"id":"prt_1","type":"step-start"}}
{"type":"text","sessionID":"ses_first","part":{"type":"text","text":"first"}}
{"type":"step_finish","sessionID":"ses_first","part":{"type":"step-finish","reason":"tool-calls","tokens":{"input":1,"output":1}}}
{"type":"step_start","sessionID":"ses_second","part":{"id":"prt_2","type":"step-start"}}
{"type":"text","sessionID":"ses_second","part":{"type":"text","text":"second"}}
{"type":"step_finish","sessionID":"ses_second","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":1}}}
`

	s := &session{
		id:      "sid-resume",
		events:  make(chan agent.Event, 16),
		rootCtx: context.Background(),
	}

	parseStream(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	if s.resumeID != "ses_first" {
		t.Errorf("resumeID: want 'ses_first', got %q", s.resumeID)
	}
}

func TestSummarizeToolInput(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash command", "Bash", `{"command":"ls -la"}`, "ls -la"},
		{"read file_path", "Read", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"edit file_path", "Edit", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"write file_path", "Write", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"glob pattern", "Glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"grep pattern", "Grep", `{"pattern":"func"}`, "func"},
		{"unknown tool", "Weirdo", `{"x":1}`, ""},
		{"nil state", "Bash", ``, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var state *toolState
			if tc.input != "" {
				state = &toolState{Input: []byte(tc.input)}
			}
			got := summarizeToolInput(tc.tool, state)
			if got != tc.want {
				t.Errorf("summarizeToolInput(%s): want %q, got %q", tc.tool, tc.want, got)
			}
		})
	}
}

func TestSummarizeToolInputLongTruncation(t *testing.T) {
	longCmd := strings.Repeat("x", 120)
	input := `{"command":"` + longCmd + `"}`
	state := &toolState{Input: []byte(input)}

	hint := summarizeToolInput("Bash", state)
	wantPrefix := strings.Repeat("x", 80)
	if !strings.HasPrefix(hint, wantPrefix) {
		t.Errorf("hint prefix: want 80 x's, got %q", hint)
	}
	if !strings.HasSuffix(hint, "...") {
		t.Errorf("hint should end with '...' on truncation, got %q", hint)
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
	if d.Name() != "opencode" {
		t.Errorf("Name: want 'opencode', got %q", d.Name())
	}
}

func TestDefaultTimeout(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	if d.timeout != defaultTimeout {
		t.Errorf("defaultTimeout: want %v, got %v", defaultTimeout, d.timeout)
	}
}

func TestCustomTimeout(t *testing.T) {
	d := New(Options{Timeout: 30 * time.Second}, obs.NewLogger())
	if d.timeout != 30*time.Second {
		t.Errorf("custom timeout: want 30s, got %v", d.timeout)
	}
}

func TestTimeoutCapsTurn(t *testing.T) {
	// Use `yes` which ignores extra args and prints 'y' forever. The
	// 10ms timeout will fire and kill the process.
	d := New(Options{Timeout: 10 * time.Millisecond}, obs.NewLogger())
	ctx := context.Background()
	sid, err := d.Start(ctx, agent.StartOpts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	d.bin = "yes"
	err = d.Send(sid, "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	ch := d.Events(sid)
	d.Stop(sid)

	var got []agent.Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) < 2 {
		t.Fatalf("want >=2 events (error + turn_end), got %d: %+v", len(got), got)
	}
	last := got[len(got)-1]
	if last.Type != agent.EvTurnEnd {
		t.Errorf("last event: want EvTurnEnd, got %v", last.Type)
	}
	hasError := false
	for _, ev := range got {
		if ev.Type == agent.EvError && strings.Contains(ev.Text, "timed out") {
			hasError = true
			break
		}
	}
	if !hasError {
		var texts []string
		for _, ev := range got {
			texts = append(texts, string(ev.Type)+":"+ev.Text)
		}
		t.Errorf("expected 'timed out' error, got events: %v", texts)
	}
}
