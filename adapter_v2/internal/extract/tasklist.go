package extract

import (
	"fmt"
	"strings"
)

// tasklist.go is ADR-034's DATA-ACQUISITION probe, not (yet) the real extractor.
//
// claude's `TodoWrite` tool renders a checklist on the TUI — each row a todo with
// a status glyph (pending / in_progress / completed). We want to surface that as a
// device "task.list" progress panel, but we do NOT yet know which exact glyph
// claude uses for each status: the screenshot that motivated this shows an empty
// ballot box "☐" (U+2610) for pending, but the in_progress / completed glyphs are
// unconfirmed, and "☐" vs "□" (U+25A1) vs "◻" (U+25FB) are different code points
// that look near-identical. ADR-034 §3 makes pinning that mapping a P0 gate that
// needs a REAL-DEVICE capture — and the render may only appear probabilistically.
//
// So this file does not classify status or emit a wire frame. It DETECTS candidate
// checklist rows with a deliberately BROAD glyph set (so we don't miss the real
// one) and hands them back with each row's leading glyph as a U+XXXX code point, so
// a single real-device hit logged via deviceapi.captureTaskListProbe gives us
// exactly what we need to build the fixture and write the strict extractor later.
//
// Strictness lives in the CALLER (a lone checkbox-looking line is likely prose, so
// deviceapi only treats a CONTIGUOUS RUN as a hit) — here we stay permissive on
// purpose. Mirrors toolstep.go's "scan the grid, return structured rows" shape.

// TaskCandidate is one screen line the probe flagged as a possible TodoWrite row.
// Status is intentionally NOT decoded (see file doc / ADR-034 §3): we capture the
// raw leading glyph so a later look at the log pins the glyph→status mapping.
type TaskCandidate struct {
	Lead   string // the leading marker as a human token: "U+2610" for a glyph, or "[ ]" / "[x]" for an ASCII checkbox
	Text   string // the row text after the marker, trimmed and rune-truncated
	Indent int    // leading spaces before the marker (TodoWrite rows are indented under the tool line)
}

// taskTextMax bounds a captured row's text (rune-safe) so a long todo can't bloat
// the probe log line. Generous vs toolHintMax — we want enough to recognise the row.
const taskTextMax = 100

// isChecklistGlyph reports whether r is a plausible TodoWrite status marker. The
// set is broad ON PURPOSE (ADR-034 §3): we don't yet know claude's real glyphs, so
// we cover ballot boxes, geometric squares/circles, and check / cross marks — but
// deliberately EXCLUDE plain bullets (•, ◦, ‣) and triangles (▶, ◀) so a prose
// bulleted list or claude's "❯ 1." selection arrow doesn't read as a checklist.
func isChecklistGlyph(r rune) bool {
	switch r {
	// Ballot boxes (the screenshot's "☐" lives here).
	case '☐', '☑', '☒': // U+2610..U+2612
		return true
	// Squares.
	case '■', '□', '▢', '▣', '◻', '◼', '◽', '◾': // U+25A0/A1/A2/A3, U+25FB..U+25FE
		return true
	// Circles (some todo renderers use ○/● for pending/done).
	case '○', '●', '◯', '◉', '◎', '⚪', '⚫': // U+25CB/CF/EF/C9/CE, U+26AA/AB
		return true
	// Check / cross marks (in_progress or completed are sometimes a ✓ / ✗ / ✅ / ❌).
	case '✓', '✔', '✕', '✖', '✗', '✘', '✅', '❌', '❎': // U+2713..U+2718, U+2705, U+274C/E
		return true
	}
	return false
}

// asciiCheckbox returns the "[ ]" / "[x]" token at the very start of s (after
// indent has already been stripped by the caller), or "" if s does not begin with
// one. Covers the markdown-task style claude may fall back to in a plain terminal.
func asciiCheckbox(s string) string {
	if len(s) < 3 || s[0] != '[' || s[2] != ']' {
		return ""
	}
	switch s[1] {
	case ' ', 'x', 'X', '-', '~', '*':
		return s[:3]
	}
	return ""
}

// ScanTaskListCandidates returns every checklist-looking row on the visible grid,
// in screen order, each with its leading marker captured verbatim as a token. It
// never decides status or block boundaries — that's the caller's (and a later
// strict extractor's) job. Empty input or no matches returns nil.
func ScanTaskListCandidates(visible string) []TaskCandidate {
	if visible == "" {
		return nil
	}
	var out []TaskCandidate
	for _, raw := range strings.Split(visible, "\n") {
		// Strip a leading "⏺ " reply marker first (the TodoWrite tool line may carry
		// it; item rows usually don't, but stripping is harmless), then measure indent.
		body := stripReplyMarker(strings.TrimRight(raw, " \t"))
		indent := len(body) - len(strings.TrimLeft(body, " "))
		s := strings.TrimLeft(body, " ")
		if s == "" {
			continue
		}

		var lead, text string
		if tok := asciiCheckbox(s); tok != "" {
			lead = tok
			text = strings.TrimSpace(s[len(tok):])
		} else {
			r := []rune(s)
			if !isChecklistGlyph(r[0]) {
				continue
			}
			lead = fmt.Sprintf("U+%04X", r[0])
			text = strings.TrimSpace(string(r[1:]))
		}
		if text == "" {
			continue // a bare glyph with no following text is not a todo row
		}
		out = append(out, TaskCandidate{Lead: lead, Text: truncateRunes(text, taskTextMax), Indent: indent})
	}
	return out
}

// LongestRun returns the longest CONTIGUOUS sub-run of cands (rows that were
// adjacent on screen) — a TodoWrite block is a solid run, so this filters out
// stray checkbox-looking prose lines scattered elsewhere. cands must be in screen
// order. It uses Indent continuity (same indent column) plus screen adjacency,
// which ScanTaskListCandidates preserves only implicitly; since we drop non-matching
// lines, "adjacency" here means consecutive entries — good enough for the probe.
func LongestRun(cands []TaskCandidate) []TaskCandidate {
	// With non-matching lines dropped, we can't see gaps between entries, so treat
	// entries sharing the SAME indent as one run (a TodoWrite block is uniformly
	// indented). This keeps a same-indent block together and splits off an unrelated
	// checkbox row at a different indent.
	best, cur := 0, 0
	bestEnd := 0
	for i := range cands {
		if i > 0 && cands[i].Indent == cands[i-1].Indent {
			cur++
		} else {
			cur = 1
		}
		if cur > best {
			best, bestEnd = cur, i+1
		}
	}
	if best == 0 {
		return nil
	}
	return cands[bestEnd-best : bestEnd]
}
