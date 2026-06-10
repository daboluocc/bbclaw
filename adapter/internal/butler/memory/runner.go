package memory

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// PromptRunner runs a single text prompt through some backend and returns the
// raw model output (ADR-024 §6). The distiller/summarizer parse a tolerant JSON
// slice out of that output, so the runner only needs to return text. This is the
// seam that lets the memory pipeline follow the active driver: the default is a
// claude `-p` runner, but main.go injects a driver-aware runner so a codex /
// opencode butler distills its memory through codex / opencode.
type PromptRunner func(ctx context.Context, prompt string) (string, error)

// ClaudePromptRunner returns a PromptRunner backed by `claude -p` on a cheap
// model (the default memory backend; preserves the Haiku cost optimisation).
func ClaudePromptRunner(bin, model string) PromptRunner {
	if strings.TrimSpace(bin) == "" {
		bin = "claude"
	}
	model = strings.TrimSpace(model)
	return func(ctx context.Context, prompt string) (string, error) {
		args := []string{"-p", prompt, "--dangerously-skip-permissions"}
		if model != "" {
			args = append(args, "--model", model)
		}
		out, err := exec.CommandContext(ctx, bin, args...).Output()
		if err != nil {
			return "", fmt.Errorf("claude -p: %w", err)
		}
		return string(out), nil
	}
}
