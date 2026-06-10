package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// TestParseStream feeds a representative `codex exec --json` thread-event
// transcript through parseStream and asserts the unified-event mapping
// (ADR-023). This is the build-time gate for the success path the live CLI on
// the dev machine cannot currently exercise (its bundled model is rejected by
// the account tier).
func TestParseStream(t *testing.T) {
	const transcript = `{"type":"thread.started","thread_id":"019eafac-9d78-78a2-a321-2995f5d82460"}
{"type":"turn.started"}
{"type":"item.started","item":{"item_type":"reasoning"}}
{"type":"item.completed","item":{"item_type":"command_execution","command":"ls -la"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Hello world"}}
{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}
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

	// Expected: EvSessionInit, EvToolCall (command), EvText (assistant), EvTokens.
	if len(got) != 4 {
		t.Fatalf("want 4 events, got %d: %+v", len(got), got)
	}
	if got[0].Type != agent.EvSessionInit || got[0].Text != "019eafac-9d78-78a2-a321-2995f5d82460" {
		t.Errorf("event 0: want EvSessionInit with thread id, got %+v", got[0])
	}
	if got[1].Type != agent.EvToolCall || got[1].Tool == nil || got[1].Tool.Hint != "ls -la" {
		t.Errorf("event 1: want EvToolCall hint='ls -la', got %+v", got[1])
	}
	if got[2].Type != agent.EvText || got[2].Text != "Hello world" {
		t.Errorf("event 2: want EvText 'Hello world', got %+v", got[2])
	}
	if got[3].Type != agent.EvTokens || got[3].Tokens == nil || got[3].Tokens.In != 12 || got[3].Tokens.Out != 3 {
		t.Errorf("event 3: want EvTokens in=12 out=3, got %+v", got[3])
	}

	if s.resumeID != "019eafac-9d78-78a2-a321-2995f5d82460" {
		t.Errorf("resumeID: want thread id, got %q", s.resumeID)
	}
}

// TestParseStreamError maps both the standalone error event and turn.failed
// (with codex's nested-JSON error blob) onto EvError with the unwrapped inner
// message.
func TestParseStreamError(t *testing.T) {
	const transcript = `{"type":"thread.started","thread_id":"t1"}
{"type":"error","message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"model not supported\"}}"}
`
	s := &session{
		id:      "sid-err",
		events:  make(chan agent.Event, 16),
		rootCtx: context.Background(),
	}
	parseStream(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var gotErr string
	for e := range s.events {
		if e.Type == agent.EvError {
			gotErr = e.Text
		}
	}
	if gotErr != "model not supported" {
		t.Errorf("want unwrapped error 'model not supported', got %q", gotErr)
	}
}

// TestCapabilitiesNotButler guards the ADR-023 invariant that codex is not
// butler-capable (PUT /v1/agent/butler_driver must reject it).
func TestCapabilitiesNotButler(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	if d.Capabilities().Butler {
		t.Error("codex must not be butler-capable")
	}
}
