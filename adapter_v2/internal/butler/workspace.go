package butler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// memoryFiles are the per-dimension long-term-memory files seeded into a fresh
// workspace. Each is created empty-but-headed so claude can read/append them with
// built-in file tools (the persona in CLAUDE.md drives this). profile.md carries
// the onboarding STATUS marker.
var memoryFiles = map[string]string{
	"profile.md": "# 用户档案\n\n<!-- STATUS: uninitialized -->\n\n- 称呼:\n- 角色 / 职业:\n- 现在主要在忙:\n",
	"preferences.md": "# 用户偏好\n\n（用户表达稳定偏好时追加要点）\n",
	"projects.md":    "# 项目与进展\n\n（用户提到的项目与进展线索追加在这里）\n",
	"decisions.md":   "# 关键决策\n\n（敲定的重要决策 + 原因，一条一句）\n",
}

// DefaultWorkspaceDir returns ~/.bbclaw-adapter-v2/workspace — the butler's home.
func DefaultWorkspaceDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, ".bbclaw-adapter-v2", "workspace"), nil
}

// EnsureWorkspace creates the butler workspace at dir (if empty, the default) and
// scaffolds CLAUDE.md + MEMORY/*.md when absent, then returns the absolute path.
// Idempotent: existing files are left untouched, so the user's accumulated memory
// and any CLAUDE.md edits survive restarts. The returned path is what the default
// session uses as its cwd, so claude loads the persona natively.
func EnsureWorkspace(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		d, err := DefaultWorkspaceDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	if err := os.MkdirAll(filepath.Join(dir, "MEMORY"), 0o755); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	claudeMD := filepath.Join(dir, "CLAUDE.md")
	if _, err := os.Stat(claudeMD); os.IsNotExist(err) {
		if err := os.WriteFile(claudeMD, []byte(defaultClaudeMD), 0o644); err != nil {
			return "", fmt.Errorf("write CLAUDE.md: %w", err)
		}
	}
	for name, body := range memoryFiles {
		p := filepath.Join(dir, "MEMORY", name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return "", fmt.Errorf("write MEMORY/%s: %w", name, err)
			}
		}
	}
	return dir, nil
}

// DeviceClaudeArgs builds the claude argv for the device/voice session. It does
// two device-specific things (scoped to a claude CLI; other CLIs pass through):
//
//  1. Bypasses permission prompts (--dangerously-skip-permissions). A voice device
//     has NO way to answer claude's "Do you want to proceed? 1.Yes 2.No" tool
//     dialogs, so a Bash/edit tool call would hang the turn forever. The butler
//     runs in its own controlled workspace, and the user opted into an autonomous
//     voice assistant, so bypassing is the right tradeoff. Disable with
//     ADAPTER_V2_SKIP_PERMISSIONS=0 (then tool turns will hang on the prompt).
//  2. Appends the walkie-talkie device persona via --append-system-prompt. Override
//     the whole prompt with ADAPTER_V2_VOICE_SYSTEM_PROMPT, or set it empty to skip.
func DeviceClaudeArgs(argv []string, cwd string) []string {
	if len(argv) == 0 || !strings.Contains(strings.ToLower(filepath.Base(argv[0])), "claude") {
		return argv
	}
	out := append([]string{}, argv...)
	if envBool("ADAPTER_V2_SKIP_PERMISSIONS", true) {
		out = append(out, "--dangerously-skip-permissions")
	}
	prompt := DeviceSystemPrompt(cwd, "")
	if v, ok := os.LookupEnv("ADAPTER_V2_VOICE_SYSTEM_PROMPT"); ok {
		prompt = v
	}
	if strings.TrimSpace(prompt) != "" {
		out = append(out, "--append-system-prompt", prompt)
	}
	return out
}

// envBool reads a boolean env var (1/true/yes/on vs 0/false/no/off), returning def
// when unset or unrecognised.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
