package claudecode

import (
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// TestParseStreamJSON_Thinking verifies that an assistant `thinking` content
// block is surfaced as EvThinking, and that a following `text` block still
// emits EvText — i.e. thinking does not swallow the reply (ADR-029 §2.2).
func TestParseStreamJSON_Thinking(t *testing.T) {
	transcript := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"先看结构再决定怎么改","signature":"sig"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"已经改好了"}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}

	if len(evs) != 2 {
		t.Fatalf("want 2 events (thinking + text), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != agent.EvThinking {
		t.Errorf("event 0: want EvThinking, got %s", evs[0].Type)
	}
	if evs[0].Text != "先看结构再决定怎么改" {
		t.Errorf("event 0: want thinking text, got %q", evs[0].Text)
	}
	if evs[1].Type != agent.EvText {
		t.Errorf("event 1: want EvText, got %s", evs[1].Type)
	}
	if evs[1].Text != "已经改好了" {
		t.Errorf("event 1: want reply text, got %q", evs[1].Text)
	}
}

// TestParseStreamJSON_ThinkingEmpty verifies an empty thinking block emits
// nothing (no zero-length EvThinking noise).
func TestParseStreamJSON_ThinkingEmpty(t *testing.T) {
	transcript := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	for e := range s.events {
		t.Errorf("want no events for empty thinking, got %+v", e)
	}
}
