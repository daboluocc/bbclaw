package butler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

func TestDeviceSessionConfigResumeModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate active-session + claude dirs
	mgr := session.NewManager()

	// A fresh workspace (no history) → start a NEW conversation (--session-id),
	// never a bare --continue (which hangs with nothing to resume).
	d := NewDeviceSession(mgr, []string{"/usr/local/bin/claude"}, "/ws")
	if a := d.Config().Argv; !containsArg(a, "--session-id") || containsArg(a, "--continue") {
		t.Errorf("fresh workspace should start a new conversation via --session-id: %v", a)
	}

	// New() → a brand-new conversation spawns with --session-id <id>.
	id := d.New()
	if id == "" {
		t.Fatal("New() returned empty id")
	}
	a := d.Config().Argv
	if i := argIndex(a, "--session-id"); i < 0 || a[i+1] != id {
		t.Errorf("New() should spawn --session-id %s: %v", id, a)
	}
	if d.ActiveID() != id {
		t.Errorf("ActiveID=%q, want %q", d.ActiveID(), id)
	}

	// A real conversation on disk so resume/reload of it is honoured (the holder
	// discards a persisted id whose .jsonl is gone).
	pdir := claudeProjectDir("/ws")
	_ = os.MkdirAll(pdir, 0o755)
	_ = os.WriteFile(filepath.Join(pdir, "abc-123.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)

	// Resume(other) → --resume <other>.
	d.Resume("abc-123")
	a = d.Config().Argv
	if i := argIndex(a, "--resume"); i < 0 || a[i+1] != "abc-123" {
		t.Errorf("Resume should spawn --resume abc-123: %v", a)
	}

	// Persistence: a fresh holder (same HOME) loads the persisted active id
	// (its .jsonl exists, so it is honoured).
	d2 := NewDeviceSession(mgr, []string{"claude"}, "/ws")
	if d2.ActiveID() != "abc-123" {
		t.Errorf("persisted active id not reloaded: got %q", d2.ActiveID())
	}

	// Non-claude CLI → no claude-specific resume flag (would break e.g. `cat`).
	dc := NewDeviceSession(mgr, []string{"cat"}, "")
	if a := dc.Config().Argv; containsArg(a, "--continue") || containsArg(a, "--resume") || containsArg(a, "--session-id") {
		t.Errorf("non-claude argv must not get a resume flag: %v", a)
	}
}

func TestDeviceSessionList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/Users/x/.bbclaw-adapter/workspace"
	// Materialise claude's project dir for cwd with two conversation files.
	dir := claudeProjectDir(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL := func(name, firstUser string) {
		body := `{"type":"user","message":{"role":"user","content":"` + firstUser + `"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONL("11111111-aaaa.jsonl", "第一段对话")
	writeJSONL("22222222-bbbb.jsonl", "second chat")

	d := NewDeviceSession(session.NewManager(), []string{"claude"}, cwd)
	list, err := d.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 conversations, got %d: %+v", len(list), list)
	}
	// Title comes from the first user message.
	titles := list[0].Title + "|" + list[1].Title
	if !strings.Contains(titles, "第一段对话") || !strings.Contains(titles, "second chat") {
		t.Errorf("titles not parsed from first user message: %q", titles)
	}
}
