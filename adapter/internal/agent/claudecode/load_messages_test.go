package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// writeTranscript emits a Claude-style JSONL conversation history file with n
// alternating user/assistant turns. Returns the file path.
func writeTranscript(t *testing.T, sessionsDir, projectsDir, sid, cwd string, n int) string {
	t.Helper()
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"sessionId": sid,
		"cwd":       cwd,
		"startedAt": int64(1714000000000),
	}
	mb, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(sessionsDir, sid+".json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := cwdToProjectDir(cwd)
	historyDir := filepath.Join(projectsDir, projectDir)
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(historyDir, sid+".jsonl")
	f, err := os.Create(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for i := 0; i < n; i++ {
		role := "user"
		typ := "user"
		content := any(fmt.Sprintf("user msg %d", i))
		if i%2 == 1 {
			role = "assistant"
			typ = "assistant"
			content = []map[string]any{{"type": "text", "text": fmt.Sprintf("assistant reply %d", i)}}
		}
		entry := map[string]any{
			"type": typ,
			"message": map[string]any{
				"role":    role,
				"content": content,
			},
		}
		row, _ := json.Marshal(entry)
		if _, err := f.Write(append(row, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	return historyPath
}

func TestLoadMessages_LatestPage(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	writeTranscript(t, sessionsDir, projectsDir, "sid-1", "/Users/test/proj", 10)

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)

	d := New(Options{}, obs.NewLogger())
	page, err := d.LoadMessages(context.Background(), "sid-1", -1, 4)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if page.Total != 10 {
		t.Errorf("total=%d, want 10", page.Total)
	}
	if !page.HasMore {
		t.Errorf("hasMore should be true")
	}
	if len(page.Messages) != 4 {
		t.Fatalf("len=%d, want 4", len(page.Messages))
	}
	if page.Messages[0].Seq != 6 || page.Messages[3].Seq != 9 {
		t.Errorf("expected seq [6..9], got [%d..%d]", page.Messages[0].Seq, page.Messages[3].Seq)
	}
	// roles alternate user/assistant; seq 6 is user (even)
	if page.Messages[0].Role != "user" {
		t.Errorf("expected role=user at seq 6, got %s", page.Messages[0].Role)
	}
	if page.Messages[1].Role != "assistant" {
		t.Errorf("expected role=assistant at seq 7, got %s", page.Messages[1].Role)
	}
	if !strings.HasPrefix(page.Messages[1].Content, "assistant reply") {
		t.Errorf("assistant content not flattened correctly: %q", page.Messages[1].Content)
	}
}

func TestLoadMessages_BeforeCursor(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	writeTranscript(t, sessionsDir, projectsDir, "sid-1", "/Users/test/proj", 10)

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	d := New(Options{}, obs.NewLogger())

	// before=6, limit=4 → window [2..5]. seq 0,1 still exist → hasMore=true.
	page, err := d.LoadMessages(context.Background(), "sid-1", 6, 4)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if !page.HasMore {
		t.Errorf("hasMore should be true (seq 0,1 not yet returned)")
	}
	if len(page.Messages) != 4 {
		t.Fatalf("len=%d, want 4", len(page.Messages))
	}
	if page.Messages[0].Seq != 2 || page.Messages[3].Seq != 5 {
		t.Errorf("expected seq [2..5], got [%d..%d]", page.Messages[0].Seq, page.Messages[3].Seq)
	}

	// before=2, limit=4 → window [0..1] (clamped at start). hasMore=false.
	last, err := d.LoadMessages(context.Background(), "sid-1", 2, 4)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if last.HasMore {
		t.Errorf("hasMore should be false when window starts at 0")
	}
	if len(last.Messages) != 2 || last.Messages[0].Seq != 0 || last.Messages[1].Seq != 1 {
		t.Errorf("expected seq [0..1], got %+v", last.Messages)
	}
}

func TestLoadMessages_BeforeBeyondTotalClampsToEnd(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	writeTranscript(t, sessionsDir, projectsDir, "sid-1", "/Users/test/proj", 5)

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	d := New(Options{}, obs.NewLogger())

	page, err := d.LoadMessages(context.Background(), "sid-1", 999, 100)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(page.Messages) != 5 || page.Total != 5 || page.HasMore {
		t.Errorf("expected full session returned: len=%d total=%d hasMore=%v", len(page.Messages), page.Total, page.HasMore)
	}
}

// Regression: an old session whose claude-code process has long exited has
// no `~/.claude/sessions/{pid}.json` metadata file, but its JSONL transcript
// still lives under `projects/{cwdHash}/{sid}.jsonl`. The earlier `findHistoryPath`
// looked the metadata up by sessionId-keyed filename, so it wrongly reported
// "no such session" and returned total=0 — observed on real hardware as
// "load_messages missing data.messages[]" + "history fetch failed".
func TestLoadMessages_NoSessionsMetadataStillFindsJSONL(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// JSONL exists but sessions/ is empty (process gone, metadata rotated).
	cwd := "/Users/test/orphaned-project"
	historyDir := filepath.Join(projectsDir, cwdToProjectDir(cwd))
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := []string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello back"}]}}`,
	}
	if err := os.WriteFile(filepath.Join(historyDir, "orphan-sid.jsonl"),
		[]byte(strings.Join(rows, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	d := New(Options{}, obs.NewLogger())
	page, err := d.LoadMessages(context.Background(), "orphan-sid", -1, 50)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("expected total=2 even without sessions metadata, got %d", page.Total)
	}
	if len(page.Messages) != 2 || page.Messages[1].Content != "hello back" {
		t.Errorf("unexpected messages: %+v", page.Messages)
	}
}

func TestLoadMessages_MissingSession(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	d := New(Options{}, obs.NewLogger())

	page, err := d.LoadMessages(context.Background(), "nope", -1, 50)
	if err != nil {
		t.Fatalf("LoadMessages should not error on missing: %v", err)
	}
	if len(page.Messages) != 0 || page.Total != 0 {
		t.Errorf("expected empty page, got len=%d total=%d", len(page.Messages), page.Total)
	}
}

// Regression (issue #41 follow-up): the adapter mints session ids with a "cc-"
// prefix (e.g. "cc-460fd894-..."), but the Claude CLI stores JSONL transcripts
// using the bare UUID as the filename ("460fd894-....jsonl"). findHistoryPath
// must strip the prefix when the prefixed name doesn't match any file on disk.
func TestLoadMessages_CcPrefixStripped(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// On-disk file uses bare UUID (as Claude CLI creates it).
	bareUUID := "460fd894-1bd6-4788-9d8d-4ad93a86b8ba"
	cwd := "/Users/test/myproject"
	historyDir := filepath.Join(projectsDir, cwdToProjectDir(cwd))
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := []string{
		`{"type":"user","message":{"role":"user","content":"hello from device"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}`,
	}
	if err := os.WriteFile(filepath.Join(historyDir, bareUUID+".jsonl"),
		[]byte(strings.Join(rows, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	d := New(Options{}, obs.NewLogger())

	// Query with "cc-" prefixed id (as stored in logical session's CLISessionID).
	page, err := d.LoadMessages(context.Background(), "cc-"+bareUUID, -1, 50)
	if err != nil {
		t.Fatalf("LoadMessages with cc- prefix: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("expected total=2 with cc- prefix lookup, got %d", page.Total)
	}
	if len(page.Messages) != 2 || page.Messages[0].Content != "hello from device" {
		t.Errorf("unexpected messages: %+v", page.Messages)
	}
}

func TestLoadMessages_SkipsToolCallsAndMalformed(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	projectsDir := filepath.Join(tmp, "projects")
	cwd := "/Users/test/proj"
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"sessionId": "sid-1", "cwd": cwd, "startedAt": int64(1714000000000)})
	_ = os.WriteFile(filepath.Join(sessionsDir, "sid-1.json"), meta, 0o644)

	historyDir := filepath.Join(projectsDir, cwdToProjectDir(cwd))
	_ = os.MkdirAll(historyDir, 0o755)

	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		`not json`,
		`{"type":"system","message":{"role":"system","content":"sys boot"}}`,
		`{"type":"tool_call","message":{}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"x"}]}}`, // no text → skipped
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"text","text":"two"}]}}`,
		`{"type":"user","message":{"role":"user","content":"second"}}`,
	}
	if err := os.WriteFile(filepath.Join(historyDir, "sid-1.jsonl"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	d := New(Options{}, obs.NewLogger())
	page, err := d.LoadMessages(context.Background(), "sid-1", -1, 50)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("expected 3 valid messages (skipped malformed/system/tool-only), got %d", page.Total)
	}
	if page.Messages[1].Content != "ok\ntwo" {
		t.Errorf("expected joined text blocks, got %q", page.Messages[1].Content)
	}
	if page.Messages[2].Content != "second" {
		t.Errorf("expected last user msg = 'second', got %q", page.Messages[2].Content)
	}
}
