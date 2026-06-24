package extract

import (
	"strings"
	"testing"
	"time"
)

// realPermissionPrompt is the VERBATIM VisibleText of claude 2.1.186's Bash
// permission menu, captured from a real session through vtscreen (ADR-033 spike).
// The hard release-gate fixture: if claude changes this UI and the parser stops
// matching, this test fails loudly.
const realPermissionPrompt = "" +
	"────────────────────────────────────────────────────\n" +
	" Bash command\n" +
	"\n" +
	"   cat /etc/hostname\n" +
	"   Print system hostname file\n" +
	"\n" +
	" Do you want to proceed?\n" +
	" ❯ 1. Yes\n" +
	"   2. Yes, allow reading from etc/ from this project\n" +
	"   3. No\n" +
	"\n" +
	" Esc to cancel · Tab to amend · ctrl+e to explain"

func TestParsePromptRealPermission(t *testing.T) {
	p, ok := ParsePrompt(realPermissionPrompt)
	if !ok {
		t.Fatal("ParsePrompt(realPermissionPrompt) = ok false, want true")
	}
	if p.Kind != PromptPermission {
		t.Errorf("Kind = %q, want permission", p.Kind)
	}
	if p.Question != "Do you want to proceed?" {
		t.Errorf("Question = %q, want %q", p.Question, "Do you want to proceed?")
	}
	if p.Mechanism != "digit" {
		t.Errorf("Mechanism = %q, want digit", p.Mechanism)
	}
	if len(p.Options) != 3 {
		t.Fatalf("len(Options) = %d, want 3: %+v", len(p.Options), p.Options)
	}
	wantKey := []string{"1", "2", "3"}
	for i, o := range p.Options {
		if o.Key != wantKey[i] {
			t.Errorf("Options[%d].Key = %q, want %q", i, o.Key, wantKey[i])
		}
	}
	if !p.Options[0].Default {
		t.Error("Options[0] (❯ 1. Yes) should be Default")
	}
	if p.Options[1].Default || p.Options[2].Default {
		t.Error("only option 1 should be Default")
	}
	if p.Options[0].Label != "Yes" || p.Options[2].Label != "No" {
		t.Errorf("labels = %q / %q, want Yes / No", p.Options[0].Label, p.Options[2].Label)
	}
}

func TestParsePromptNegatives(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"busy spinner":     busyScreen,
		"idle prompt":      idleScreen,
		"prompt with text": promptWithUserText,
		// A prose numbered list in a reply: numbered rows but NO pointer and NO
		// "Esc to cancel" footer → must not be mistaken for a menu.
		"prose numbered list": "⏺ Here are the steps:\n" +
			"  1. First clone the repo\n" +
			"  2. Then run make build\n" +
			"  3. Finally flash it\n" +
			"╭────────────────╮\n│ >              │\n╰────────────────╯",
		// Bare "❯" idle prompt glyph must not look like an option (no digit).
		"idle pointer glyph": "⏺ Done.\n\n❯ \n",
		// Single option is not a menu.
		"single option": " Do you want to proceed?\n ❯ 1. Yes\n Esc to cancel",
	}
	for name, screen := range cases {
		if p, ok := ParsePrompt(screen); ok {
			t.Errorf("ParsePrompt(%s) = ok true, want false: %+v", name, p)
		}
	}
}

func TestParsePromptEditConfirmKind(t *testing.T) {
	screen := "" +
		" Edit file\n   internal/foo.go\n\n" +
		" Do you want to make this edit to foo.go?\n" +
		" ❯ 1. Yes\n" +
		"   2. Yes, allow all edits this session\n" +
		"   3. No, and tell Claude what to do differently\n\n" +
		" Esc to cancel"
	p, ok := ParsePrompt(screen)
	if !ok {
		t.Fatal("edit-confirm prompt not parsed")
	}
	if p.Kind != PromptEditConfirm {
		t.Errorf("Kind = %q, want editConfirm", p.Kind)
	}
	if len(p.Options) != 3 {
		t.Errorf("len(Options) = %d, want 3", len(p.Options))
	}
}

// Pointer glyph dropped by the emulator, but the "Esc to cancel" footer still
// corroborates the menu; option 1 must be marked default as a fallback.
func TestParsePromptPointerlessButFooter(t *testing.T) {
	screen := " Do you want to proceed?\n" +
		"   1. Yes\n   2. No\n\n Esc to cancel · Tab to amend"
	p, ok := ParsePrompt(screen)
	if !ok {
		t.Fatal("footer-corroborated menu not parsed")
	}
	if !p.Options[0].Default {
		t.Error("option 1 should be defaulted when no pointer glyph present")
	}
}

// Signature changes when claude appends/rewrites a context-scoped option, so the
// caller can supersede the old promptId (design §4).
func TestParsePromptSignatureSupersede(t *testing.T) {
	a, _ := ParsePrompt(realPermissionPrompt)
	changed := strings.Replace(realPermissionPrompt,
		"2. Yes, allow reading from etc/ from this project",
		"2. Yes, allow reading from /usr/ from this project", 1)
	b, ok := ParsePrompt(changed)
	if !ok {
		t.Fatal("changed prompt not parsed")
	}
	if a.Signature == b.Signature {
		t.Errorf("signature should differ when an option label changes: %q", a.Signature)
	}
}

// §0 regression (ADR-033): a blocking menu clears the spinner, but the turn is
// parked — TurnEnded MUST stay false even though a spinner was seen this turn and
// output has settled. Before the fix the sawSpinner short-circuit returned true.
func TestBoundaryAwaitingPromptNotTurnEnd(t *testing.T) {
	d := &Detector{}
	t0 := time.Now()
	// Turn runs: spinner appears (sets sawSpinner), then the permission menu paints
	// and the spinner is gone.
	d.Observe(t0, fakeScreen{busyScreen}, true)
	d.Observe(t0.Add(50*time.Millisecond), fakeScreen{realPermissionPrompt}, true)
	// Output settles well past Quiet.
	now := t0.Add(50*time.Millisecond + 5*Quiet)
	if d.TurnEnded(now) {
		t.Fatal("TurnEnded = true while a blocking prompt is on screen (ADR-033 §0 regression)")
	}
	// Once answered (menu gone, idle prompt back), the turn may end.
	d.Observe(now, fakeScreen{idleScreen}, true)
	if !d.TurnEnded(now.Add(2 * Quiet)) {
		t.Error("TurnEnded = false after the menu cleared and output settled")
	}
}
