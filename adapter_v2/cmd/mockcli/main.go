// Command mockcli is a tiny stand-in for a real agent TUI (claude / codex / …)
// used by the device end-to-end smoke (see adapter_v2/internal/deviceapi e2e
// test and the Makefile `e2e` target). It is a REAL upstream process driven over
// a PTY exactly like the production CLI would be — not an inline shell snippet —
// so the smoke exercises the genuine spawn → inject → reply → boundary → TTS path
// with no external dependencies (no network, no installed agent binary).
//
// It imitates just enough of an agent TUI's screen behaviour for v2's turn
// boundary detector (internal/extract/boundary.go) to fire cleanly:
//
//   - it paints an idle input prompt that collapses to a bare ">" so the detector
//     sees the CLI ready for input (boundary heuristic: idle prompt returned);
//   - on each line read from stdin it paints a "working" spinner line carrying the
//     literal "esc to interrupt" affordance (heuristic: turn in flight), then the
//     reply line "ANSWER: <input>", then ERASES the spinner and reprints the idle
//     ">" prompt and goes quiet (heuristics: spinner gone + output settled).
//
// Positioning uses absolute cursor moves so the reply / spinner / prompt land on
// distinct, stable rows that the server-side VT mirror and the noise classifier
// handle cleanly.
//
// Faithfulness note — leading ESC: deviceapi.SubmitVoiceTurn prepends an ESC (the
// interrupt key) before the transcript. A real raw-mode CLI consumes ESC as a
// distinct keypress, so it never becomes part of the typed text. A line-buffered
// reader (this program's bufio.Scanner over a PTY in canonical mode) would
// instead capture that leading ESC into the line, so we strip a single leading
// ESC to model the real CLI's behaviour; otherwise the captured ESC would re-enter
// the VT mirror as the start of an escape sequence and swallow the next byte.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// VT control sequences. Kept as named constants so the screen choreography below
// reads as intent ("move to the prompt row, clear it, paint '> '") rather than a
// wall of escape bytes.
const (
	esc = "\x1b"

	// promptRow is where the idle "> " input prompt lives. spinnerRow and replyRow
	// are deliberately on different, stable rows so they never overwrite each other
	// in the VT mirror.
	cursorToPrompt  = esc + "[5;1H"
	cursorToReply   = esc + "[7;1H"
	cursorToSpinner = esc + "[10;1H"

	clearLine = esc + "[2K" // erase the current line

	// spinnerLine carries the stable "esc to interrupt" affordance the boundary
	// detector keys off as the "still working" signal. The leading dim attribute
	// (\x1b[2m … \x1b[0m) mimics a real CLI's faint spinner styling; the VT mirror
	// strips the SGR and the noise classifier matches on the text.
	spinnerLine = esc + "[2m. Working... (1s · esc to interrupt)" + esc + "[0m"

	// idlePrompt is the empty, ready-for-input box the detector requires to see
	// before it will declare a turn finished. It collapses to a bare ">" in the
	// VT mirror after box borders (if any) are stripped.
	idlePrompt = "> "

	// replyPrefix marks the canned reply so the smoke can assert the round-trip
	// text deterministically: the spoken reply must contain "ANSWER: <transcript>".
	replyPrefix = "ANSWER: "
)

func main() {
	out := bufio.NewWriter(os.Stdout)

	// Paint the initial idle prompt so a bridge attaching before the first turn
	// already sees the CLI "ready" baseline, matching a freshly launched agent TUI.
	fmt.Fprint(out, cursorToPrompt+idlePrompt)
	_ = out.Flush()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		// Strip a single leading ESC: see the package doc — SubmitVoiceTurn's
		// interrupt key is consumed as a keypress by a real raw-mode CLI, but a
		// line-buffered reader captures it.
		line = strings.TrimPrefix(line, esc)

		// 1. Show the working spinner (turn now "in flight").
		fmt.Fprint(out, cursorToSpinner+clearLine+spinnerLine)
		// 2. Paint the reply on its own row.
		fmt.Fprint(out, cursorToReply+clearLine+replyPrefix+line)
		// 3. Clear the spinner and return the idle prompt (turn finished). The CLI
		//    then goes quiet, so the boundary detector's settle window can elapse.
		fmt.Fprint(out, cursorToSpinner+clearLine)
		fmt.Fprint(out, cursorToPrompt+clearLine+idlePrompt)
		_ = out.Flush()
	}
}
