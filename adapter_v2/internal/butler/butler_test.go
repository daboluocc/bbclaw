package butler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendDevicePersona(t *testing.T) {
	// claude CLI → device persona appended via --append-system-prompt.
	got := AppendDevicePersona([]string{"/usr/local/bin/claude"}, "/ws")
	if len(got) != 3 || got[1] != "--append-system-prompt" || !strings.Contains(got[2], "对讲机") {
		t.Errorf("claude argv not augmented: %v", got)
	}
	// Non-claude CLI (e.g. the e2e mockcli) → untouched.
	if base := AppendDevicePersona([]string{"/tmp/mockcli"}, "/ws"); len(base) != 1 {
		t.Errorf("non-claude argv should be untouched, got %v", base)
	}
	// Explicit empty env disables the prompt.
	t.Setenv("ADAPTER_V2_VOICE_SYSTEM_PROMPT", "")
	if off := AppendDevicePersona([]string{"claude"}, ""); len(off) != 1 {
		t.Errorf("empty env should disable the persona, got %v", off)
	}
	// Custom env overrides the default whole prompt.
	t.Setenv("ADAPTER_V2_VOICE_SYSTEM_PROMPT", "be terse")
	if cust := AppendDevicePersona([]string{"claude"}, ""); len(cust) != 3 || cust[2] != "be terse" {
		t.Errorf("custom env not applied, got %v", cust)
	}
}

func TestEnsureWorkspaceScaffoldsAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	got, err := EnsureWorkspace(dir)
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if got != dir {
		t.Fatalf("returned %q, want %q", got, dir)
	}
	// CLAUDE.md carries the butler persona; MEMORY/profile.md carries the onboarding
	// status marker so claude has REAL (file-based) memory, not a hallucinated one.
	md, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(md), "BBClaw 管家") {
		t.Fatalf("CLAUDE.md missing/wrong: err=%v", err)
	}
	prof := filepath.Join(dir, "MEMORY", "profile.md")
	if b, err := os.ReadFile(prof); err != nil || !strings.Contains(string(b), "STATUS: uninitialized") {
		t.Fatalf("MEMORY/profile.md missing status marker: err=%v", err)
	}

	// Idempotent: a user's edits + accumulated memory must survive a restart.
	custom := "# my edited persona\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWorkspace(dir); err != nil {
		t.Fatalf("second EnsureWorkspace: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != custom {
		t.Errorf("EnsureWorkspace clobbered an existing CLAUDE.md")
	}
}
