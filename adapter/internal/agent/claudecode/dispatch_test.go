package claudecode

import (
	"context"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// TestParseStreamJSON_MCPDispatch_Done verifies that a mcp__bbclaw__dispatch
// tool_use followed by a done tool_result emits two EvDispatchStatus events
// (started, done) and does NOT emit EvToolCall.
func TestParseStreamJSON_MCPDispatch_Done(t *testing.T) {
	transcript := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01","name":"mcp__bbclaw__dispatch","input":{"project":"bbclaw","task":"重构 auth 模块"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"{\"status\":\"done\",\"result\":\"ok\"}"}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}

	// Expect exactly 2 EvDispatchStatus events
	if len(evs) != 2 {
		t.Fatalf("want 2 events (started + done), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != agent.EvDispatchStatus {
		t.Errorf("event 0: want EvDispatchStatus, got %s", evs[0].Type)
	}
	if evs[0].Dispatch == nil || evs[0].Dispatch.Phase != "started" {
		t.Errorf("event 0: want phase=started, got %+v", evs[0].Dispatch)
	}
	if evs[0].Dispatch.TaskID != "toolu_01" {
		t.Errorf("event 0: want taskId=toolu_01, got %q", evs[0].Dispatch.TaskID)
	}
	if evs[0].Dispatch.Cwd != "bbclaw" {
		t.Errorf("event 0: want cwd=bbclaw, got %q", evs[0].Dispatch.Cwd)
	}
	if evs[0].Dispatch.Title == "" {
		t.Errorf("event 0: want non-empty title")
	}

	if evs[1].Type != agent.EvDispatchStatus {
		t.Errorf("event 1: want EvDispatchStatus, got %s", evs[1].Type)
	}
	if evs[1].Dispatch == nil || evs[1].Dispatch.Phase != "done" {
		t.Errorf("event 1: want phase=done, got %+v", evs[1].Dispatch)
	}
}

// TestParseStreamJSON_MCPDispatch_Async verifies the async (running→async) phase.
func TestParseStreamJSON_MCPDispatch_Async(t *testing.T) {
	transcript := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_02","name":"mcp__bbclaw__dispatch","input":{"cwd":"/home/user/proj","task":"lint all files"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_02","content":"{\"status\":\"running\",\"taskId\":\"task-abc\"}"}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	if evs[1].Dispatch == nil || evs[1].Dispatch.Phase != "async" {
		t.Errorf("event 1: want phase=async, got %+v", evs[1].Dispatch)
	}
	// taskId should come from butlermcp result, not tool_use.id
	if evs[1].Dispatch.TaskID != "task-abc" {
		t.Errorf("event 1: want taskId=task-abc, got %q", evs[1].Dispatch.TaskID)
	}
}

// TestParseStreamJSON_MCPDispatch_Error verifies the error phase.
func TestParseStreamJSON_MCPDispatch_Error(t *testing.T) {
	transcript := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_03","name":"mcp__bbclaw__dispatch","input":{"project":"docs","task":"update ADR"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_03","is_error":true,"content":"{\"status\":\"error\",\"taskId\":\"task-xyz\",\"error\":\"timeout\"}"}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	if evs[1].Dispatch == nil || evs[1].Dispatch.Phase != "error" {
		t.Errorf("event 1: want phase=error, got %+v", evs[1].Dispatch)
	}
	if evs[1].Dispatch.Error != "timeout" {
		t.Errorf("event 1: want error=timeout, got %q", evs[1].Dispatch.Error)
	}
}

// TestParseStreamJSON_NonMCPToolUse_StillEmitsEvToolCall verifies that
// non-mcp__bbclaw__ tool_use frames still emit EvToolCall (no regression).
func TestParseStreamJSON_NonMCPToolUse_StillEmitsEvToolCall(t *testing.T) {
	transcript := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_bash","name":"Bash","input":{"command":"ls -la"}}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != agent.EvToolCall {
		t.Errorf("want EvToolCall for Bash, got %s", evs[0].Type)
	}
	if evs[0].Tool == nil || evs[0].Tool.Tool != "Bash" {
		t.Errorf("want tool=Bash, got %+v", evs[0].Tool)
	}
}

// TestParseStreamJSON_NonMCPToolResult_Discarded verifies that tool_results
// not matching a tracked mcp dispatch are silently discarded.
func TestParseStreamJSON_NonMCPToolResult_Discarded(t *testing.T) {
	transcript := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_unknown","content":"some result"}]}}
`
	s := newTestSession()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 0 {
		t.Errorf("want 0 events for non-MCP tool_result, got %d: %+v", len(evs), evs)
	}
}

func newTestSession() *session {
	return &session{
		id:      "sid-test",
		events:  make(chan agent.Event, 32),
		rootCtx: context.Background(),
	}
}
