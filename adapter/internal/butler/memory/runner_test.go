package memory

import (
	"context"
	"strings"
	"testing"
)

// TestRunnerDistillerUsesInjectedRunner verifies the distiller calls the
// injected PromptRunner (ADR-024 §6) rather than shelling to claude, and parses
// the returned JSON — i.e. memory distillation follows whatever backend the
// runner wraps (claude / codex / opencode).
func TestRunnerDistillerUsesInjectedRunner(t *testing.T) {
	var gotPrompt string
	fake := func(ctx context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		// codex/opencode wrap the JSON in prose; parseItems must tolerate it.
		return "sure, here you go:\n[{\"category\":\"preference\",\"text\":\"喜欢简短回复\"}]\nok", nil
	}
	d := newRunnerDistiller(fake)

	items, err := d.Distill(context.Background(), "我喜欢简短", "好的")
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if !strings.Contains(gotPrompt, "我喜欢简短") || !strings.Contains(gotPrompt, "好的") {
		t.Errorf("prompt should embed the turn, got %q", gotPrompt)
	}
	if len(items) != 1 || items[0].Category != "preference" || items[0].Text != "喜欢简短回复" {
		t.Fatalf("want 1 preference item, got %+v", items)
	}
}

// TestRunnerSummarizerUsesInjectedRunner verifies the consolidate path is also
// driver-agnostic.
func TestRunnerSummarizerUsesInjectedRunner(t *testing.T) {
	fake := func(ctx context.Context, prompt string) (string, error) {
		return `{"preference":["喜欢简短回复"],"project":[],"decision":[]}`, nil
	}
	s := newRunnerSummarizer(fake, 6)
	dims, err := s.Summarize(context.Background(), "收件箱内容", map[string]string{})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(dims["preference"]) != 1 || dims["preference"][0] != "喜欢简短回复" {
		t.Errorf("want 1 preference bullet, got %+v", dims)
	}
}
