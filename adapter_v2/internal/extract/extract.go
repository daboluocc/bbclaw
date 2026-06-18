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

// extract computes the newest-reply text from the current screen: take the
// visible content lines (noise already removed), drop any that were present in
// the baseline, and join the remainder. Leading/trailing blank lines are
// trimmed; trailing whitespace per line is trimmed (the acceptance criterion
// allows trailing-whitespace differences).
func (e *Extractor) extract() string {
	lines := contentLines(e.screen.VisibleText())

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
		out = append(out, strings.TrimRight(l, " \t"))
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
