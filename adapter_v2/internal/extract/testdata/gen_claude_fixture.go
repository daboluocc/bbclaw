//go:build ignore

// gen_claude_fixture.go regenerates the recorded claude-style TUI byte stream
// used by the extract package tests.
//
// Run it from the extract package directory:
//
//	cd adapter_v2/internal/extract
//	/opt/homebrew/bin/go run testdata/gen_claude_fixture.go
//
// It writes testdata/claude_reply.vt — the raw bytes a PTY would deliver while
// `claude` (no -p, interactive TUI) answers a single prompt. We synthesise the
// bytes rather than capturing a live session so the fixture is deterministic and
// reproducible offline (a live capture drifts with model output, timing, token
// counts and claude's UI revisions; this script is the documented spec of the
// TUI shape we strip, and is trivial to update when claude changes its UI).
//
// What the stream reproduces, faithfully to claude's primary-screen TUI:
//
//   - the rounded box-drawing input prompt (╭─ … ─╮ / │ > … │ / ╰─ … ─╯) that
//     sits at the bottom and is the boundary marker between turns;
//   - a spinner / status line ("✶ Cogitating… (Ns · ↑ N tokens · esc to
//     interrupt)") redrawn IN PLACE many times via CR + erase-line, so naive
//     diffing would emit duplicate/jittering frames;
//   - the assistant reply streamed as plain text bulleted with "⏺ ", arriving in
//     several chunks (as a real stream would), interleaved with spinner redraws;
//   - SGR colour around the reply and box (claude dims the box, colours the
//     bullet) to prove colour is stripped from the extracted text.
//
// The companion testdata/claude_reply.expected.txt holds the human-annotated
// reply text the extractor must recover from this stream.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Terminal geometry the fixture was authored for. The tests feed the stream
// into a vtscreen.Screen of exactly this size.
const (
	cols = 80
	rows = 24
)

// ANSI helpers — kept tiny and explicit so the fixture reads like a transcript.
const (
	esc      = "\x1b"
	reset    = esc + "[0m"
	dim      = esc + "[2m"
	cyan     = esc + "[36m"
	clearAll = esc + "[2J" + esc + "[H" // erase screen, home cursor
	eraseLn  = esc + "[2K"              // erase entire line
	cr       = "\r"
	crlf     = "\r\n"
)

// cup moves the cursor to (row,col), 1-based, the way claude positions its
// redraws.
func cup(row, col int) string { return fmt.Sprintf("%s[%d;%dH", esc, row, col) }

// promptBox renders claude's rounded input prompt occupying the bottom three
// rows of the screen. `text` is whatever the user has typed (empty = idle box).
// The box is drawn dim, matching claude.
func promptBox(text string) string {
	inner := cols - 4 // chars between the "│ " and " │" borders
	line := " > " + text
	if len(line) > inner {
		line = line[:inner]
	}
	line += strings.Repeat(" ", inner-len(line))
	var b strings.Builder
	b.WriteString(dim)
	b.WriteString(cup(rows-2, 1) + eraseLn + "╭" + strings.Repeat("─", cols-2) + "╮")
	b.WriteString(cup(rows-1, 1) + eraseLn + "│" + line + "│")
	b.WriteString(cup(rows, 1) + eraseLn + "╰" + strings.Repeat("─", cols-2) + "╯")
	b.WriteString(reset)
	return b.String()
}

// spinnerFrame draws the status/spinner line in place at row rows-3. claude
// cycles a glyph and ticks an elapsed-seconds + token counter; we redraw the
// same line via CR + erase so it overwrites itself (the classic source of
// duplicate/jitter text if an extractor diffs naively).
func spinnerFrame(glyph string, secs, tokens int) string {
	status := fmt.Sprintf("%s Cogitating… (%ds · ↑ %d tokens · esc to interrupt)", glyph, secs, tokens)
	return cup(rows-3, 1) + cr + eraseLn + dim + status + reset
}

func main() {
	var b strings.Builder

	// Fresh screen with the idle prompt box, as claude shows at rest.
	b.WriteString(clearAll)
	b.WriteString(promptBox(""))

	// User has "typed" a question into the box (shown inside the prompt). This
	// text lives in the input region and MUST be stripped from the reply.
	b.WriteString(promptBox("what is the capital of France?"))

	// Spinner glyphs claude rotates through while working.
	spin := []string{"✶", "✻", "✽", "✻"}

	// A few spinner-only redraws before any reply text streams in. These must
	// NOT surface as reply text.
	for i := 0; i < 4; i++ {
		b.WriteString(spinnerFrame(spin[i%len(spin)], i, 120+i*40))
	}

	// The reply streams into the conversation region (above the spinner line),
	// chunk by chunk, with spinner redraws interleaved — exactly the racey
	// ordering a real PTY delivers. The reply is bulleted with "⏺ " and the
	// bullet is coloured; the body is plain. Lines are placed explicitly so the
	// fixture is independent of wrapping.
	type chunk struct {
		row  int
		col  int
		text string // includes its own SGR; no trailing newline
	}
	// Newest assistant reply (this is what the extractor must isolate).
	reply := []chunk{
		{row: 3, col: 1, text: cyan + "⏺" + reset + " The capital of France is Paris."},
		{row: 4, col: 1, text: "  It has been the country's capital since the 12th century"},
		{row: 5, col: 1, text: "  and is its largest city."},
	}

	// Simulate an OLDER turn already on screen above the new reply, to prove the
	// extractor isolates only the NEWEST reply via the baseline diff. (Row 1 is
	// a faded previous answer; the diff baseline is taken after it is drawn.)
	b.WriteString(cup(1, 1) + dim + "⏺ (earlier) 2 + 2 = 4." + reset)

	// --- BASELINE POINT -------------------------------------------------------
	// The test snapshots VisibleText() here as the extractor's baseline; only
	// content drawn AFTER this marker is "new". We encode the marker as an
	// otherwise-inert OSC the test can split on, then strip. Using OSC keeps it
	// out of the visible grid entirely (vtscreen ignores it).
	b.WriteString(esc + "]1337;baseline\x07")

	// Stream the reply chunk by chunk, redrawing the spinner between chunks so
	// the fixture exercises "reply text + spinner churn arriving interleaved".
	for i, c := range reply {
		b.WriteString(cup(c.row, c.col) + eraseLn + c.text)
		b.WriteString(spinnerFrame(spin[(i+1)%len(spin)], 4+i, 300+i*60))
	}

	// More spinner-only frames after the text settled but before the turn ends
	// (claude keeps the spinner alive briefly). These redraw the SAME status
	// line; a correct extractor yields stable text across them.
	for i := 0; i < 6; i++ {
		b.WriteString(spinnerFrame(spin[i%len(spin)], 7+i, 480+i*30))
	}

	// Turn ends: claude clears the spinner line and the prompt box returns to
	// idle (empty), the signal #210/boundary.go keys off of. We clear the
	// spinner row and redraw the idle box.
	b.WriteString(cup(rows-3, 1) + eraseLn)
	b.WriteString(promptBox(""))

	out := []byte(b.String())

	dir, err := scriptDir()
	if err != nil {
		fail(err)
	}
	path := filepath.Join(dir, "claude_reply.vt")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s (%d bytes, %dx%d)\n", path, len(out), cols, rows)
}

// scriptDir returns the directory this script lives in (testdata/), regardless
// of the working directory `go run` was invoked from.
func scriptDir() (string, error) {
	// `go run` compiles to a temp dir, so os.Args[0] is unhelpful; resolve via
	// the current working directory + known layout instead. We expect to be run
	// from the extract package dir (the documented invocation) OR from testdata.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if filepath.Base(wd) == "testdata" {
		return wd, nil
	}
	td := filepath.Join(wd, "testdata")
	if st, err := os.Stat(td); err == nil && st.IsDir() {
		return td, nil
	}
	return "", fmt.Errorf("run from the extract package dir or its testdata/ (cwd=%s)", wd)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen_claude_fixture:", err)
	os.Exit(1)
}
