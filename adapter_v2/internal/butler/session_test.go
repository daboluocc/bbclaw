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

func TestDeviceSessionDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mgr := session.NewManager()
	cwd := "/ws"
	pdir := claudeProjectDir(cwd)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(id, text string) {
		body := `{"type":"user","message":{"role":"user","content":"` + text + `"}}` + "\n"
		if err := os.WriteFile(filepath.Join(pdir, id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep-1", "one")
	write("keep-2", "two")
	write("gone-3", "three")

	d := NewDeviceSession(mgr, []string{"claude"}, cwd)
	d.Resume("keep-1") // make keep-1 the active conversation

	// Delete a NON-active conversation: file removed, active untouched.
	if active, err := d.Delete("gone-3"); err != nil || active != "keep-1" {
		t.Fatalf("Delete(non-active) = (%q,%v), want (keep-1,nil)", active, err)
	}
	if _, err := os.Stat(filepath.Join(pdir, "gone-3.jsonl")); !os.IsNotExist(err) {
		t.Errorf("gone-3.jsonl should be removed, stat err=%v", err)
	}

	// Deleting a missing conversation is idempotent (no error).
	if _, err := d.Delete("gone-3"); err != nil {
		t.Errorf("Delete(missing) should be nil, got %v", err)
	}

	// Delete the ACTIVE conversation: active moves to the most-recent remaining
	// (keep-2, which was written after keep-1), never a now-missing id.
	active, err := d.Delete("keep-1")
	if err != nil {
		t.Fatalf("Delete(active) err=%v", err)
	}
	if active != "keep-2" || d.ActiveID() != "keep-2" {
		t.Errorf("after deleting active, active=%q/%q, want keep-2", active, d.ActiveID())
	}
	if _, err := os.Stat(filepath.Join(pdir, "keep-1.jsonl")); !os.IsNotExist(err) {
		t.Errorf("keep-1.jsonl should be removed, stat err=%v", err)
	}

	// Delete the last remaining conversation: no history left → mint a fresh one
	// (a brand-new --session-id), not an empty/continue active.
	active, err = d.Delete("keep-2")
	if err != nil {
		t.Fatalf("Delete(last) err=%v", err)
	}
	if active == "" || active == "keep-2" {
		t.Errorf("deleting the last conversation should mint a fresh id, got %q", active)
	}
	if a := d.Config().Argv; argIndex(a, "--session-id") < 0 {
		t.Errorf("fresh conversation after last delete should spawn --session-id: %v", a)
	}
}

func TestDeviceSessionMessages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/Users/x/.bbclaw-adapter/workspace"
	dir := claudeProjectDir(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Mixed rows: a plain user turn, a user tool_result (no text → dropped), an
	// assistant text turn, and an assistant thinking-only turn (dropped).
	lines := []string{
		`{"type":"user","timestamp":"2026-06-23T00:00:01Z","message":{"role":"user","content":"问题一"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"junk"}]}}`,
		`{"type":"assistant","timestamp":"2026-06-23T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"回答一"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}}`,
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "conv-1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDeviceSession(session.NewManager(), []string{"claude"}, cwd)
	msgs, err := d.Messages("conv-1")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 speakable messages (tool_result + thinking dropped), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "问题一" || msgs[0].Seq != 0 {
		t.Errorf("msg0 wrong: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "回答一" || msgs[1].Seq != 1 {
		t.Errorf("msg1 wrong: %+v", msgs[1])
	}
	if msgs[1].Timestamp != "2026-06-23T00:00:02Z" {
		t.Errorf("timestamp not parsed: %q", msgs[1].Timestamp)
	}
	// Missing conversation → empty page, not an error.
	if m, err := d.Messages("nope"); err != nil || len(m) != 0 {
		t.Errorf("missing conversation should be empty: %v %v", m, err)
	}
}

func TestPageMessages(t *testing.T) {
	mk := func(n int) []ConversationMessage {
		out := make([]ConversationMessage, n)
		for i := range out {
			out[i] = ConversationMessage{Seq: i}
		}
		return out
	}
	all := mk(10)
	// Newest page (before<=0): last `limit`.
	page, total, more := PageMessages(all, 0, 4)
	if total != 10 || len(page) != 4 || page[0].Seq != 6 || !more {
		t.Errorf("newest page wrong: total=%d len=%d first=%d more=%v", total, len(page), page[0].Seq, more)
	}
	// Backward page ending before seq 6 → seqs 2..5.
	page, _, more = PageMessages(all, 6, 4)
	if len(page) != 4 || page[0].Seq != 2 || page[3].Seq != 5 || !more {
		t.Errorf("backward page wrong: %+v more=%v", page, more)
	}
	// Reaching the start → no more.
	page, _, more = PageMessages(all, 3, 10)
	if len(page) != 3 || page[0].Seq != 0 || more {
		t.Errorf("start page wrong: %+v more=%v", page, more)
	}
}

func TestListSkipsV1PoolNoopProbes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/Users/x/.bbclaw-adapter/workspace"
	dir := claudeProjectDir(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	noop := `{"type":"user","message":{"role":"user","content":"respond with the single word: ready"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ready"}]}}` + "\n"
	real := `{"type":"user","message":{"role":"user","content":"记住我叫周老板,我做硬件"}}` + "\n"
	for _, n := range []string{"noop-aaaa", "noop-bbbb", "noop-cccc"} {
		if err := os.WriteFile(filepath.Join(dir, n+".jsonl"), []byte(noop), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "real-1.jsonl"), []byte(real), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDeviceSession(session.NewManager(), []string{"claude"}, cwd)
	list, _ := d.List()
	if len(list) != 1 || list[0].ID != "real-1" {
		t.Fatalf("List must drop v1 pool noop probes, keep only the real conversation; got %+v", list)
	}
	// The active-pick must land on the real conversation, not a noop probe.
	if d.ActiveID() != "real-1" {
		t.Errorf("active session should be the real conversation, got %q", d.ActiveID())
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
