package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writePartsTranscript writes a transcript exercising every part kind:
// a user turn, an assistant turn with thinking+text+dispatch, and a later
// user row carrying the dispatch tool_result (which must fold into the
// dispatch part, not become a visible turn).
func writePartsTranscript(t *testing.T, sessionsDir, projectsDir, sid, cwd string) {
	t.Helper()
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, sid+".json"), []byte(`{"sessionId":"`+sid+`","cwd":"`+cwd+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	historyDir := filepath.Join(projectsDir, cwdToProjectDir(cwd))
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"user","message":{"role":"user","content":"重构 auth"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"先看结构"},{"type":"text","text":"我来重构，先派个 worker"},{"type":"tool_use","id":"toolu_1","name":"mcp__bbclaw__dispatch","input":{"cwd":"bbclaw","prompt":"重构 token 刷新"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"{\"status\":\"done\",\"taskId\":\"task-9\",\"elapsedMs\":8200}"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"重构完成"}]}}
`
	if err := os.WriteFile(filepath.Join(historyDir, sid+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadParts_StructuredTurns(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	writePartsTranscript(t, sessionsDir, projectsDir, "sid-1", "/Users/test/proj")
	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)

	d := &Driver{}
	page, err := d.LoadParts(context.Background(), "sid-1", -1, 50)
	if err != nil {
		t.Fatalf("LoadParts: %v", err)
	}

	// 3 visible turns: user, assistant(thinking+text+dispatch), assistant(text).
	// The tool_result-only user row must NOT appear.
	if len(page.Turns) != 3 {
		t.Fatalf("want 3 visible turns, got %d: %+v", len(page.Turns), page.Turns)
	}

	if page.Turns[0].Role != "user" || len(page.Turns[0].Parts) != 1 || page.Turns[0].Parts[0].Kind != "text" {
		t.Errorf("turn 0: want user/text, got %+v", page.Turns[0])
	}

	asst := page.Turns[1]
	if asst.Role != "assistant" {
		t.Fatalf("turn 1: want assistant, got %s", asst.Role)
	}
	if len(asst.Parts) != 3 {
		t.Fatalf("turn 1: want 3 parts (thinking,text,dispatch), got %d: %+v", len(asst.Parts), asst.Parts)
	}
	if asst.Parts[0].Kind != "thinking" || asst.Parts[0].Text != "先看结构" {
		t.Errorf("part 0: want thinking '先看结构', got %+v", asst.Parts[0])
	}
	if asst.Parts[1].Kind != "text" {
		t.Errorf("part 1: want text, got %+v", asst.Parts[1])
	}
	dp := asst.Parts[2]
	if dp.Kind != "dispatch" || dp.Dispatch == nil {
		t.Fatalf("part 2: want dispatch, got %+v", dp)
	}
	if dp.Dispatch.Status != "done" {
		t.Errorf("dispatch: want status=done (folded from tool_result), got %q", dp.Dispatch.Status)
	}
	if dp.Dispatch.TaskID != "task-9" {
		t.Errorf("dispatch: want taskId=task-9, got %q", dp.Dispatch.TaskID)
	}
	if dp.Dispatch.ElapsedMs != 8200 {
		t.Errorf("dispatch: want elapsedMs=8200, got %d", dp.Dispatch.ElapsedMs)
	}
	if dp.Dispatch.Cwd != "bbclaw" {
		t.Errorf("dispatch: want cwd=bbclaw, got %q", dp.Dispatch.Cwd)
	}
}
