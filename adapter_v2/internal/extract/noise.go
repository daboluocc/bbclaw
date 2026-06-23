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

// apiRetryHints mark claude's network/API retry wait — "Waiting for API response
// · will retry in Ns · check your network". In this state claude drops the
// "esc to interrupt" affordance, but the turn is NOT finished: it will retry.
// The boundary detector must treat it as still-working, else it completes the
// turn early and clears the in-flight flag — so a barge-in then TYPES into the
// input box (claude queues it: "Press up to edit queued messages") instead of
// ESC-aborting. These substrings are the stable markers of that state.
var apiRetryHints = []string{"will retry", "Waiting for API response"}

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
	if isStatusLine(t) {
		return true
	}
	if isBoxDrawingOnly(t) {
		return true
	}
	return false
}

// isStatusLine matches claude's persistent footer / completion chrome that sits
// below the reply and whose values mutate every turn (so it slips past the diff
// baseline): the "✻ Worked for Ns" completion summary, the "[Opus … (… context)]"
// model line, the "Context …" / "Usage …" meters, and the "… for agents" hint.
// These are never reply content, so dropping them keeps the spoken text clean.
func isStatusLine(t string) bool {
	// A leading dingbat star (U+2722–U+2747) is claude's animated spinner /
	// completion-summary glyph: "✻ Cogitating…", "✶ Worked for 2s", etc. The verb
	// and glyph both cycle, but the leading star is stable, so a range check is
	// far more robust than blocklisting each phrase.
	if r := firstRune(t); r >= 0x2722 && r <= 0x2747 {
		return true
	}
	switch {
	case strings.Contains(t, "Worked for") || strings.Contains(t, "Cogitated for"):
		return true // completion summary without a star glyph
	case strings.HasPrefix(t, "Context ") || strings.HasPrefix(t, "Usage "):
		return true // footer progress meters
	case strings.HasPrefix(t, "[") && strings.Contains(t, "context)]"):
		return true // "[Opus 4.8 (1M context)] │ …" model status
	case strings.Contains(t, "for agents"):
		return true // "… ← for agents" footer hint
	case strings.Contains(t, "tokens") && (strings.HasSuffix(t, "tokens)") ||
		strings.ContainsRune(t, '↑') || strings.ContainsRune(t, '↓') || strings.Contains(t, "·")):
		return true // token-counter fragment ("38 tokens)", "↑ 1.2k tokens · …") —
		// the completion summary's counter wraps onto its own flush-left line and
		// would otherwise leak onto the end of the spoken reply.
	}
	return false
}

// firstRune returns the first rune of s, or 0 for the empty string.
func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// isPromptLine matches the CLI input line where the user types. claude renders
// it as "│ > …" inside the prompt box; once box borders are stripped by the
// emulator it collapses to "> …". We also treat a bare ">" (empty idle prompt)
// and the placeholder hint claude shows in an empty box as prompt chrome.
func isPromptLine(t string) bool {
	// Strip a leading left-border glyph if the emulator kept it.
	t = strings.TrimLeft(t, "│|")
	t = strings.TrimSpace(t)
	// claude renders the prompt glyph as "❯" (U+276F); older/other CLIs use a
	// plain ">". Match both, empty (idle) or holding the user's typed text.
	for _, p := range []string{">", "❯"} {
		if t == p {
			return true // empty idle prompt
		}
		if strings.HasPrefix(t, p+" ") {
			return true // "❯ what the user typed"
		}
	}
	return false
}

// isSpinnerLine matches the working/progress status line — claude is busy and the
// turn is in flight. The "esc to interrupt" affordance is the stable marker of the
// normal working spinner; the API-retry wait (no "esc to interrupt" but still
// working — see apiRetryHints) is the other busy state. Both keep the boundary
// detector from ending the turn (and keep barge-in's in-flight ESC armed).
func isSpinnerLine(t string) bool {
	if strings.Contains(t, promptInterruptHint) {
		return true
	}
	for _, h := range apiRetryHints {
		if strings.Contains(t, h) {
			return true
		}
	}
	return false
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
