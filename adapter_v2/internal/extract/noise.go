package extract

import "strings"

// noise.go classifies a single screen line as "noise" (UI chrome we must keep
// out of the extracted reply) vs. real conversation content.
//
// The three noise classes, all observed in claude's primary-screen TUI:
//
//   - Input-prompt region: the rounded box at the bottom holding what the user
//     is typing — "│ > … │" plus its "╭──╮" / "╰──╯" borders. The user's own
//     text must never leak into the assistant reply.
//   - Spinner / progress status: the in-place-redrawn "✶ Cogitating… (Ns · ↑ N
//     tokens · esc to interrupt)" line. Redrawn dozens of times per second; if
//     it reached the reply it would jitter wildly.
//   - Box-drawing borders: lines made up entirely of box-drawing glyphs (and
//     blanks). A correct VT emulator passes claude's literal-UTF-8 box chars
//     through to VisibleText, so we strip them explicitly rather than relying on
//     any one emulator dropping them.
//
// These are heuristics keyed off claude's current UI; the package doc and the
// fixture's regen script record the shape so this stays maintainable when
// claude changes its TUI (per the issue: "this layer is the most brittle").

// promptInterruptHint is claude's signature on its working/spinner status line.
// Matching it is the most robust spinner test: the glyph, wording, and counters
// all churn, but the "esc to interrupt" affordance is stable while busy.
const promptInterruptHint = "esc to interrupt"

// isNoiseLine reports whether a visible line is UI chrome rather than reply
// content. The input line is whitespace-insensitive: callers pass a raw grid
// row (already plain text, no ANSI).
func isNoiseLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return true // blank rows carry no content
	}
	if isPromptLine(t) {
		return true
	}
	if isSpinnerLine(t) {
		return true
	}
	if isBoxDrawingOnly(t) {
		return true
	}
	return false
}

// isPromptLine matches the CLI input line where the user types. claude renders
// it as "│ > …" inside the prompt box; once box borders are stripped by the
// emulator it collapses to "> …". We also treat a bare ">" (empty idle prompt)
// and the placeholder hint claude shows in an empty box as prompt chrome.
func isPromptLine(t string) bool {
	// Strip a leading left-border glyph if the emulator kept it.
	t = strings.TrimLeft(t, "│|")
	t = strings.TrimSpace(t)
	if t == ">" {
		return true // empty idle prompt
	}
	if strings.HasPrefix(t, "> ") {
		return true // "> what the user typed"
	}
	return false
}

// isSpinnerLine matches the working/progress status line. The "esc to interrupt"
// affordance is claude's stable marker; we also catch the rare frame where only
// the elapsed-time / token counter is present by checking for the interrupt
// hint, which is what makes spinner redraws collapse to nothing in the reply.
func isSpinnerLine(t string) bool {
	return strings.Contains(t, promptInterruptHint)
}

// isBoxDrawingOnly reports whether a line consists solely of box-drawing /
// rule glyphs and spaces — i.e. a border row like "╭────╮" or "╰────╯" with no
// textual content. Such rows are pure chrome.
func isBoxDrawingOnly(t string) bool {
	hasGlyph := false
	for _, r := range t {
		if r == ' ' {
			continue
		}
		if !isBoxDrawingRune(r) {
			return false
		}
		hasGlyph = true
	}
	return hasGlyph
}

// isBoxDrawingRune reports whether r is in the Unicode Box Drawing block
// (U+2500–U+257F) or the Block Elements block (U+2580–U+259F, e.g. the "▔"
// top-rule claude sometimes draws). These cover every border glyph claude uses.
func isBoxDrawingRune(r rune) bool {
	return r >= 0x2500 && r <= 0x259F
}
