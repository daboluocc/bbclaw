package butler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceClaudeArgs(t *testing.T) {
	// claude CLI → bypass-permissions flag + device persona appended.
	got := DeviceClaudeArgs([]string{"/usr/local/bin/claude"}, "/ws")
	if !containsArg(got, "--dangerously-skip-permissions") {
		t.Errorf("missing permission bypass (voice device can't answer prompts): %v", got)
	}
	if i := argIndex(got, "--append-system-prompt"); i < 0 || i+1 >= len(got) || !strings.Contains(got[i+1], "对讲机") {
		t.Errorf("device persona not appended: %v", got)
	}
	// The resume flag is NOT part of the base argv — DeviceSession owns it.
	if containsArg(got, "--continue") || containsArg(got, "--resume") {
		t.Errorf("base argv must not carry a resume flag (DeviceSession adds it): %v", got)
	}
	// Non-claude CLI (e.g. the e2e mockcli) → untouched.
	if base := DeviceClaudeArgs([]string{"/tmp/mockcli"}, "/ws"); len(base) != 1 {
		t.Errorf("non-claude argv should be untouched, got %v", base)
	}
	// Permission bypass can be turned off for safety.
	t.Setenv("ADAPTER_V2_SKIP_PERMISSIONS", "0")
	if off := DeviceClaudeArgs([]string{"claude"}, ""); containsArg(off, "--dangerously-skip-permissions") {
		t.Errorf("ADAPTER_V2_SKIP_PERMISSIONS=0 should drop the bypass flag: %v", off)
	}
	t.Setenv("ADAPTER_V2_SKIP_PERMISSIONS", "1")
	// Custom persona env overrides the default whole prompt.
	t.Setenv("ADAPTER_V2_VOICE_SYSTEM_PROMPT", "be terse")
	got = DeviceClaudeArgs([]string{"claude"}, "")
	if i := argIndex(got, "--append-system-prompt"); i < 0 || got[i+1] != "be terse" {
		t.Errorf("custom persona env not applied: %v", got)
	}
}

// TestDeviceClaudeArgsConfirmOnDevice: forward-to-device mode (ADR-033) drops the
// bypass and forces --permission-mode default so claude renders its permission
// menus (which the device confirms). It takes precedence over SKIP_PERMISSIONS.
func TestDeviceClaudeArgsConfirmOnDevice(t *testing.T) {
	t.Setenv("ADAPTER_V2_CONFIRM_ON_DEVICE", "1")
	t.Setenv("ADAPTER_V2_SKIP_PERMISSIONS", "1") // ConfirmOnDevice must still win
	got := DeviceClaudeArgs([]string{"claude"}, "")
	if containsArg(got, "--dangerously-skip-permissions") {
		t.Errorf("ConfirmOnDevice must NOT bypass permissions: %v", got)
	}
	if i := argIndex(got, "--permission-mode"); i < 0 || i+1 >= len(got) || got[i+1] != "default" {
		t.Errorf("ConfirmOnDevice must add --permission-mode default: %v", got)
	}
}

func containsArg(args []string, want string) bool { return argIndex(args, want) >= 0 }
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
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
