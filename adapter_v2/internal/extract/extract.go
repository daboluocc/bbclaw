// Package extract is channel ② — it scrapes the server-side VT screen for the
// assistant's reply text so the BBClaw device/voice line gets clean plain text
// instead of raw TUI bytes. This is the hardest, most brittle layer in v2:
// stream-json gave us structured events for free; here we reverse-engineer a UI
// meant for human eyes.
//
// Accepted scope (decided): emit plain reply text + coarse busy/idle status
// only. We do NOT attempt to recover thinking blocks, precise tool-approval
// events, dispatch progress, or token counts.
//
// # How extraction works
//
// On every PTY chunk the screen is Fed and then OnOutput is called. We:
//
//  1. Read the whole visible grid as plain text (vtscreen.VisibleText).
//  2. Drop "noise" lines — the CLI input-prompt box region, the spinner /
//     progress status line, and box-drawing-only borders — leaving just the
//     conversation content (see noise.go for the per-line classifier).
//  3. Diff the surviving content against the baseline captured when the
//     Extractor was created, so only the NEWEST reply (the lines that appeared
//     after the user's last turn) is kept, not the scrollback above it.
//  4. Emit a Reply only when that extracted text actually changed. Because the
//     spinner is stripped in step 2, the dozens of spinner redraw frames a real
//     `claude` session emits collapse to identical extracted text and produce no
//     duplicate / jittering Replies.
//
// Complete is intentionally left false here: deciding when the turn has ended is
// boundary.go's job (#210). This package only proposes the content.
package extract

import (
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// Reply is one extracted assistant turn destined for TTS / the device screen.
type Reply struct {
	Text     string
	Complete bool // true once boundary detection judges the turn finished
}

// Extractor watches a Screen and emits Replies as new reply text stabilises. It
// is fed the same byte stream as the screen (the caller Feeds the screen, then
// calls OnOutput). It is not goroutine-safe; the owning Session serialises
// access, mirroring vtscreen.Screen.
type Extractor struct {
	screen *vtscreen.Screen

	// baseline is the set of content lines already on screen when the Extractor
	// was created (e.g. an earlier turn's reply, or nothing). Lines present in
	// the baseline are treated as "old" and excluded from the extracted reply so
	// we surface only the newest turn. Stored as a set for O(1) membership.
	baseline map[string]struct{}

	// lastText is the most recently emitted reply text, used to suppress
	// duplicate emissions across spinner redraws and unrelated repaints.
	lastText string
}

// New builds an Extractor bound to a screen and snapshots the screen's current
// content as the baseline, so the first reply OnOutput surfaces is only what is
// drawn after this point. Construct it the moment a new user turn is injected.
func New(s *vtscreen.Screen) *Extractor {
	e := &Extractor{screen: s, baseline: map[string]struct{}{}}
	for _, line := range contentLines(s.VisibleText()) {
		e.baseline[line] = struct{}{}
	}
	return e
}

// OnOutput is called after each PTY chunk is Fed to the screen. It returns the
// current best extraction of the newest assistant reply and a bool reporting
// whether that text changed since the last emission. The bool is false for
// spinner-only redraws and any repaint that does not alter the reply text, so
// callers can ignore churn and only act on genuinely new content.
//
// Complete is always false here; boundary.go (#210) decides turn completion.
func (e *Extractor) OnOutput() (Reply, bool) {
	text := e.extract()
	if text == e.lastText {
		return Reply{Text: text}, false
	}
	e.lastText = text
	return Reply{Text: text}, true
}

// replyMarker is claude's assistant-turn bullet (U+23FA): every assistant block
// is rendered "⏺ <text>" at column 0, continuation lines indented two spaces.
const replyMarker = "⏺"

// extract computes the newest-reply text from the current screen. Preferred path:
// anchor on claude's "⏺" reply marker and take that block (#claude). Fallback for
// CLIs without the marker: diff the visible content against the per-turn baseline
// so only the newest lines survive.
func (e *Extractor) extract() string {
	visible := e.screen.VisibleText()
	if reply, ok := extractMarkerBlock(visible); ok {
		return reply
	}

	lines := contentLines(visible)
	// Keep only lines not already present in the baseline. A reply line that
	// happens to duplicate baseline text verbatim is rare and harmless to drop;
	// isolating the newest turn matters more for the device/voice line.
	kept := lines[:0:0]
	for _, l := range lines {
		if _, old := e.baseline[l]; old {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// extractMarkerBlock isolates the newest claude assistant reply by anchoring on
// the "⏺" bullet: take the LAST marker line (bullet stripped) plus the indented
// continuation lines that follow it, stopping at the first non-indented line or
// noise line (spinner/status/prompt/box-rule). This keeps the surrounding footer
// chrome and the "✻ … for Ns" completion summary out of the reply, regardless of
// how their values churn. ok is false when no marker is on screen (a non-claude
// CLI), so the caller falls back to diff-based extraction.
func extractMarkerBlock(visible string) (string, bool) {
	raw := strings.Split(visible, "\n")
	last := -1
	for i, l := range raw {
		if strings.HasPrefix(strings.TrimLeft(l, " "), replyMarker) {
			last = i
		}
	}
	if last < 0 {
		return "", false
	}

	block := []string{stripReplyMarker(strings.TrimRight(raw[last], " \t"))}
	for k := last + 1; k < len(raw); k++ {
		l := strings.TrimRight(raw[k], " \t")
		if l == "" {
			block = append(block, "") // keep interior blanks (paragraph breaks)
			continue
		}
		// A continuation line is indented; anything flush-left, or any noise line,
		// ends the reply block (the "✻ … for Ns" summary, box rules, prompt, footer).
		if isNoiseLine(l) || !strings.HasPrefix(raw[k], "  ") {
			break
		}
		block = append(block, l)
	}
	for len(block) > 0 && block[len(block)-1] == "" {
		block = block[:len(block)-1]
	}
	return strings.Join(block, "\n"), true
}

// stripReplyMarker removes claude's assistant-turn bullet ("⏺ ", U+23FA) from the
// start of a line so the extracted/spoken text is clean prose. The marker only
// leads the first line of a reply; continuation lines (claude indents them with
// spaces) carry no marker and are returned untouched, so their indentation is
// preserved. A line whose only lead is the marker collapses to its text.
func stripReplyMarker(l string) string {
	if rest, ok := strings.CutPrefix(strings.TrimLeft(l, " "), "⏺"); ok {
		return strings.TrimLeft(rest, " ")
	}
	return l
}

// contentLines splits VisibleText into lines, drops noise lines (prompt region,
// spinner/status, box-drawing borders, blanks) per isNoiseLine, and trims each
// surviving line's trailing whitespace. Leading and trailing blank-equivalent
// lines are removed; interior structure is preserved.
func contentLines(visible string) []string {
	if visible == "" {
		return nil
	}
	raw := strings.Split(visible, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if isNoiseLine(l) {
			continue
		}
		out = append(out, stripReplyMarker(strings.TrimRight(l, " \t")))
	}
	// Trim leading/trailing empties left behind after noise removal.
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
