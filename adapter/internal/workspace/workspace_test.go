package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// redirectDataDir points DataDir() at an isolated temp dir for the test.
func redirectDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BBCLAW_DATA_DIR", dir)
	return dir
}

func TestEnsureScaffoldFirstRunWritesDefault(t *testing.T) {
	redirectDataDir(t)

	dir, err := EnsureScaffold()
	if err != nil {
		t.Fatalf("EnsureScaffold: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workspace dir missing: %v", err)
	}

	path := filepath.Join(dir, "CLAUDE.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("CLAUDE.md not created: %v", err)
	}
	content := string(body)

	// Persona must carry the dispatcher identity, the MCP tools, and the
	// device constraints (acceptance: persona content review anchors).
	for _, want := range []string{"调度器", "list_projects", "dispatch", "task_status", "task_result", "PTT", "可朗读"} {
		if !strings.Contains(content, want) {
			t.Errorf("default CLAUDE.md missing %q", want)
		}
	}
	if !HasManagedBlock(content) {
		t.Errorf("default CLAUDE.md must contain a managed marker block")
	}
}

func TestEnsureScaffoldIdempotentDoesNotClobber(t *testing.T) {
	redirectDataDir(t)

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("first EnsureScaffold: %v", err)
	}
	path, err := ClaudeMDPath()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the user hand-editing CLAUDE.md, including inside the managed
	// block — a re-run must leave every byte untouched.
	userContent := "# my own persona\n\nhello world\n\n" + ManagedBegin + "\nkeep me\n" + ManagedEnd + "\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("second EnsureScaffold: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userContent {
		t.Errorf("EnsureScaffold clobbered user content:\nwant %q\ngot  %q", userContent, string(got))
	}
}

func TestEnsureScaffoldAppendsMissingMarker(t *testing.T) {
	redirectDataDir(t)

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("first EnsureScaffold: %v", err)
	}
	path, err := ClaudeMDPath()
	if err != nil {
		t.Fatal(err)
	}

	// User replaced CLAUDE.md with content that has NO managed block.
	userContent := "# only my stuff\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("re-run EnsureScaffold: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.HasPrefix(gotStr, userContent) {
		t.Errorf("user content must be preserved as prefix, got:\n%s", gotStr)
	}
	if !HasManagedBlock(gotStr) {
		t.Errorf("missing managed block should have been appended, got:\n%s", gotStr)
	}
}

func TestFindAndReplaceManagedBlock(t *testing.T) {
	base := "intro\n" + ManagedBegin + "\nold note\n" + ManagedEnd + "\noutro\n"

	inner, ok := ManagedBlock(base)
	if !ok {
		t.Fatal("expected to find managed block")
	}
	if inner != "old note" {
		t.Errorf("inner = %q, want %q", inner, "old note")
	}

	updated := ReplaceManagedBlock(base, "new note")
	gotInner, ok := ManagedBlock(updated)
	if !ok || gotInner != "new note" {
		t.Errorf("after replace inner = %q (ok=%v), want %q", gotInner, ok, "new note")
	}
	// Content outside the block must survive replacement.
	if !strings.HasPrefix(updated, "intro\n") || !strings.HasSuffix(updated, "outro\n") {
		t.Errorf("replacement disturbed surrounding content:\n%s", updated)
	}
}

func TestReplaceManagedBlockAppendsWhenAbsent(t *testing.T) {
	base := "no markers here"
	updated := ReplaceManagedBlock(base, "first note")
	if !HasManagedBlock(updated) {
		t.Fatalf("expected a managed block to be appended:\n%s", updated)
	}
	inner, _ := ManagedBlock(updated)
	if inner != "first note" {
		t.Errorf("inner = %q, want %q", inner, "first note")
	}
	if !strings.HasPrefix(updated, "no markers here") {
		t.Errorf("original content must be preserved:\n%s", updated)
	}
}

func TestFindManagedBlockRejectsMalformed(t *testing.T) {
	// END before BEGIN, or only one marker, is not a valid block.
	for _, content := range []string{
		ManagedEnd + "\n" + ManagedBegin,
		ManagedBegin + " only",
		ManagedEnd + " only",
		"nothing",
	} {
		if HasManagedBlock(content) {
			t.Errorf("content should not be a valid block: %q", content)
		}
	}
}

func TestEnsureScaffoldCreatesMemoryDimensions(t *testing.T) {
	redirectDataDir(t)

	dir, err := EnsureScaffold()
	if err != nil {
		t.Fatalf("EnsureScaffold: %v", err)
	}

	memDir := filepath.Join(dir, "MEMORY")
	info, err := os.Stat(memDir)
	if err != nil {
		t.Fatalf("MEMORY dir missing: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("MEMORY is not a directory")
	}

	if len(MemoryDimensions) == 0 {
		t.Fatal("MemoryDimensions must not be empty")
	}
	for _, dim := range MemoryDimensions {
		file := filepath.Join(memDir, dim.File)
		fi, err := os.Stat(file)
		if err != nil {
			t.Errorf("dimension file %s not created: %v", dim.File, err)
			continue
		}
		// Memory content is sensitive → 0600.
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perm = %o, want 0600", dim.File, perm)
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("read %s: %v", dim.File, err)
			continue
		}
		if string(body) != dim.Skeleton {
			t.Errorf("%s skeleton = %q, want %q", dim.File, string(body), dim.Skeleton)
		}
	}
}

func TestEnsureScaffoldDoesNotClobberMemory(t *testing.T) {
	redirectDataDir(t)

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("first EnsureScaffold: %v", err)
	}

	// User (or the consolidation engine) wrote real content into one file.
	prefPath, err := MemoryFilePath(MemoryDimensions[0].File)
	if err != nil {
		t.Fatal(err)
	}
	custom := "# 用户长期偏好\n\n- 喜欢简洁回答\n"
	if err := os.WriteFile(prefPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("second EnsureScaffold: %v", err)
	}
	got, err := os.ReadFile(prefPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Errorf("EnsureScaffold clobbered MEMORY content:\nwant %q\ngot  %q", custom, string(got))
	}
}

func TestEnsureScaffoldRebuildsMissingMemoryFile(t *testing.T) {
	redirectDataDir(t)

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("first EnsureScaffold: %v", err)
	}

	// Delete a single dimension file; a re-run must recreate just that one.
	target, err := MemoryFilePath(MemoryDimensions[0].File)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureScaffold(); err != nil {
		t.Fatalf("re-run EnsureScaffold: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("missing dimension file not rebuilt: %v", err)
	}
	if string(body) != MemoryDimensions[0].Skeleton {
		t.Errorf("rebuilt file = %q, want skeleton %q", string(body), MemoryDimensions[0].Skeleton)
	}
}

func TestDefaultPersonaCarriesMemoryIndex(t *testing.T) {
	// The persona must advertise the on-demand memory index and still keep a
	// well-formed managed block so the memory pipeline's append anchor holds.
	for _, want := range []string{
		"长期记忆（按需读取）",
		"MEMORY/preferences.md",
		"MEMORY/projects.md",
		"MEMORY/decisions.md",
	} {
		if !strings.Contains(DefaultClaudeMD, want) {
			t.Errorf("DefaultClaudeMD missing %q", want)
		}
	}
	if !HasManagedBlock(DefaultClaudeMD) {
		t.Error("DefaultClaudeMD must still contain a managed marker block")
	}
	// Index entries must reference every dimension file by name.
	for _, dim := range MemoryDimensions {
		if !strings.Contains(DefaultClaudeMD, "MEMORY/"+dim.File) {
			t.Errorf("persona index missing dimension %q", dim.File)
		}
	}
}

func TestMemoryPathHelpers(t *testing.T) {
	dir := redirectDataDir(t)

	memDir, err := MemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	wantMem := filepath.Join(dir, "workspace", "MEMORY")
	if memDir != wantMem {
		t.Errorf("MemoryDir = %q, want %q", memDir, wantMem)
	}

	fp, err := MemoryFilePath("preferences.md")
	if err != nil {
		t.Fatal(err)
	}
	wantFP := filepath.Join(wantMem, "preferences.md")
	if fp != wantFP {
		t.Errorf("MemoryFilePath = %q, want %q", fp, wantFP)
	}
}

func TestDataDirHonoursEnv(t *testing.T) {
	dir := redirectDataDir(t)
	got, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("DataDir = %q, want %q", got, dir)
	}
	wsDir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if wsDir != filepath.Join(dir, "workspace") {
		t.Errorf("Dir = %q, want %q", wsDir, filepath.Join(dir, "workspace"))
	}
}
