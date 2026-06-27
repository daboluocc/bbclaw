package extract

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

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

// interruptedBanner is claude's redirect line after ONE ESC interrupts a running
// generation: "Interrupted · What should Claude do instead?" with the user's
// message moved into the composer. The turn is NEITHER finished NOR a clean idle
// prompt — claude is parked waiting for the user to redirect. Two consumers key
// off this (ADR-041): (1) the boundary detector must NOT read it as turn-end (the
// bare "❯" below it otherwise satisfies isIdlePromptLine); (2) the device
// clean-cancel path must detect it to dismiss it and reach a true idle prompt.
// The stable substring is "what should claude do instead" — the leading
// "Interrupted ·" and the middot can transcode/wrap; match case-insensitively.
const interruptedBanner = "what should claude do instead"

// HasInterruptedBanner reports whether claude's post-ESC "Interrupted · What
// should Claude do instead?" redirect prompt is on the visible grid (ADR-041).
func HasInterruptedBanner(visible string) bool {
	return strings.Contains(strings.ToLower(visible), interruptedBanner)
}

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
	case isTokenCounterOnly(t):
		return true // a line that is ONLY the completion summary's token counter
		// ("38 tokens)", "↑ 1.2k tokens · ↓ 3 tokens") — it wraps onto its own
		// flush-left line and would otherwise leak onto the spoken reply. NOTE: we
		// must NOT reject a real reply line that merely has token chrome glued to its
		// tail (claude renders "⏺ <short reply>  ↓ N tokens)" on one row) — that line
		// is prose, recovered as a reply, with the chrome stripped by NormalizeReply.
	}
	return false
}

// counterRune reports whether r is part of token-counter chrome — digits, the
// "1.2k"/"3m" unit letters, arrows, the "·" separator, parens, and spaces — i.e.
// everything in "(↑ 1.2k tokens · ↓ 3 tokens)" except the word "tokens" itself.
func counterRune(r rune) bool {
	switch r {
	case ' ', '.', '·', '↑', '↓', '(', ')', 'k', 'K', 'm', 'M':
		return true
	}
	return r >= '0' && r <= '9'
}

// isCounterChrome reports whether s is ONLY token-counter chrome: nothing but the
// word "tokens" and counterRunes. "↑ 1.2k tokens" / "38 tokens)" → true;
// "100 tokens 可用" → false (the trailing prose is not chrome).
func isCounterChrome(s string) bool {
	for _, r := range strings.ReplaceAll(s, "tokens", "") {
		if !counterRune(r) {
			return false
		}
	}
	return true
}

// stripTokenCounter removes a trailing token-counter fragment ("↓ 3 tokens)",
// "(↑ 1.2k tokens · ↓ 3 tokens)") that claude wraps onto the END of a short reply
// line, so it is neither mistaken for a pure status line nor spoken. It strips ONLY
// when the suffix from the fragment start to end-of-line is pure counter chrome and
// carries a digit — so a reply that legitimately says "tokens" mid-sentence
// ("你的余额还有 100 tokens 可用") is left untouched.
func stripTokenCounter(t string) string {
	idx := strings.Index(t, "tokens")
	if idx < 0 {
		return t
	}
	start := idx
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(t[:start])
		if !counterRune(r) {
			break
		}
		start -= size
	}
	suffix := t[start:]
	if !isCounterChrome(suffix) || !strings.ContainsAny(suffix, "0123456789") {
		return t // "tokens" sits inside prose, not a counter — leave the line alone
	}
	return strings.TrimRight(t[:start], " ")
}

// isTokenCounterOnly reports whether the whole line is token-counter chrome (so it
// should be dropped as noise), as opposed to a reply line that merely ends with it.
func isTokenCounterOnly(t string) bool {
	return strings.Contains(t, "tokens") && strings.TrimSpace(stripTokenCounter(t)) == ""
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

// optionLineRe matches one numbered select-menu row in claude's blocking popups,
// e.g. "❯ 1. Yes", "  2. Yes, allow …", "  3. No". The optional leading "❯"/"›"
// is claude's highlight pointer (the default row). Group 1 = pointer (or ""),
// group 2 = digit key, group 3 = label. Anchored on the "digit + . / )" shape so
// a prose line ("1990 was …") without the dot-space never matches. Lives here in
// noise.go with the other claude-UI line classifiers so a TUI change is fixed in
// one place (prompt.go builds on it).
var optionLineRe = regexp.MustCompile(`^[ \t]*([❯›])?[ \t]*(\d+)[.)][ \t]+(\S.*?)[ \t]*$`)

// parseOptionLine extracts {pointer, key, label} from a numbered menu row, or
// ok=false. pointer reports whether the highlight glyph ("❯"/"›") precedes it.
func parseOptionLine(raw string) (pointer bool, key, label string, ok bool) {
	m := optionLineRe.FindStringSubmatch(raw)
	if m == nil {
		return false, "", "", false
	}
	return m[1] != "", m[2], strings.TrimSpace(m[3]), true
}

// isOptionLine reports whether t is a numbered select-menu option row.
func isOptionLine(t string) bool {
	_, _, _, ok := parseOptionLine(t)
	return ok
}
