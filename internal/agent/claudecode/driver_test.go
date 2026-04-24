package claudecode

import (
	"context"
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
{"type":"assistant","message":{"content":[{"type":"text","text":" world"},{"type":"tool_use","id":"tu_1","name":"Bash"}]}}
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

	if len(got) != 3 {
		t.Fatalf("want 3 events (2 text + 1 tokens), got %d: %+v", len(got), got)
	}

	if got[0].Type != agent.EvText || got[0].Text != "Hello" {
		t.Errorf("event 0: want EvText 'Hello', got %+v", got[0])
	}
	if got[1].Type != agent.EvText || got[1].Text != " world" {
		t.Errorf("event 1: want EvText ' world', got %+v", got[1])
	}
	if got[2].Type != agent.EvTokens || got[2].Tokens == nil {
		t.Fatalf("event 2: want EvTokens, got %+v", got[2])
	}
	if got[2].Tokens.In != 12 || got[2].Tokens.Out != 3 {
		t.Errorf("tokens: want in=12 out=3, got %+v", got[2].Tokens)
	}

	// Resume ID should have been captured from the init event.
	if s.resumeID != "abc-123" {
		t.Errorf("resumeID: want 'abc-123', got %q", s.resumeID)
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
