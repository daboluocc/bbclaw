package vtscreen

import (
	"strings"
	"testing"
)

// feedStr is a tiny helper so tests can write ANSI as Go strings.
func feedStr(s *Screen, chunks ...string) {
	for _, c := range chunks {
		s.Feed([]byte(c))
	}
}

// TestVisibleText drives a variety of ANSI (cursor moves, SGR, erases, wrapping)
// and asserts the flattened visible grid matches the expected plain text.
func TestVisibleText(t *testing.T) {
	tests := []struct {
		name string
		cols int
		rows int
		feed []string
		want string
	}{
		{
			name: "plain text",
			cols: 20, rows: 4,
			feed: []string{"hello world"},
			want: "hello world",
		},
		{
			name: "newline advances row, CR returns to col0",
			cols: 20, rows: 4,
			feed: []string{"line1\r\nline2"},
			want: "line1\nline2",
		},
		{
			name: "absolute cursor positioning overwrites",
			cols: 20, rows: 4,
			// Move to row2,col2 (CUP is 1-based), print "X"; then row1,col1 "AB".
			feed: []string{"\x1b[2;2HX", "\x1b[1;1HAB"},
			want: "AB\n X",
		},
		{
			name: "SGR bold/color does not leak into plain text",
			cols: 20, rows: 2,
			feed: []string{"\x1b[1;31mRED\x1b[0m done"},
			want: "RED done",
		},
		{
			name: "erase-in-line from cursor clears tail",
			cols: 20, rows: 2,
			// print "abcdef", move cursor back to col4 (CHA), erase to EOL.
			feed: []string{"abcdef", "\x1b[4G", "\x1b[0K"},
			want: "abc",
		},
		{
			name: "carriage return overwrite (progress-bar style)",
			cols: 20, rows: 2,
			feed: []string{"50%\r100%"},
			want: "100%",
		},
		{
			name: "trailing blank rows trimmed",
			cols: 10, rows: 5,
			feed: []string{"only one line"},
			// "only one line" is 13 chars; at 10 cols it wraps after col 10
			// ("only one l") into a second grid row ("ine"); rest blank/trimmed.
			want: "only one l\nine",
		},
		{
			name: "cursor up then overwrite",
			cols: 20, rows: 4,
			feed: []string{"a\r\nb\r\nc", "\x1b[2A", "\x1b[1GZ"},
			want: "Z\nb\nc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cols, tc.rows)
			feedStr(s, tc.feed...)
			got := s.VisibleText()
			if got != tc.want {
				t.Errorf("VisibleText mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestSnapshotReplayable is the core acceptance test: after feeding a stream,
// Snapshot() written to a fresh Screen must yield an equivalent VisibleText.
func TestSnapshotReplayable(t *testing.T) {
	tests := []struct {
		name string
		cols int
		rows int
		feed []string
	}{
		{
			name: "plain multiline",
			cols: 30, rows: 6,
			feed: []string{"first line\r\nsecond line\r\nthird"},
		},
		{
			name: "with SGR attributes and colors",
			cols: 40, rows: 5,
			feed: []string{
				"\x1b[1mBold\x1b[0m normal ",
				"\x1b[31mred\x1b[0m ",
				"\x1b[4munder\x1b[0m\r\n",
				"\x1b[38;5;200mxterm256\x1b[0m\r\n",
				"\x1b[38;2;10;20;30mtruecolor\x1b[0m",
			},
		},
		{
			name: "cursor jumps and partial erases",
			cols: 25, rows: 6,
			feed: []string{
				"AAAAA\r\nBBBBB\r\nCCCCC",
				"\x1b[1;3H", "\x1b[0K", // erase tail of row1 from col3
				"\x1b[2;1HZZ", // overwrite start of row2
			},
		},
		{
			name: "box drawing / unicode",
			cols: 20, rows: 4,
			feed: []string{"┌───┐\r\n│ x │\r\n└───┘"},
		},
		{
			name: "wide CJK characters",
			cols: 20, rows: 3,
			feed: []string{"日本語テスト\r\nおわり"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := New(tc.cols, tc.rows)
			feedStr(orig, tc.feed...)
			wantText := orig.VisibleText()

			snap := orig.Snapshot()
			if len(snap) == 0 {
				t.Fatal("Snapshot returned empty bytes")
			}

			replay := New(tc.cols, tc.rows)
			replay.Feed(snap)
			gotText := replay.VisibleText()

			if gotText != wantText {
				t.Errorf("replayed VisibleText differs from original\noriginal: %q\nreplayed: %q\nsnapshot: %q",
					wantText, gotText, string(snap))
			}
		})
	}
}

// TestSnapshotPreservesCursor checks the snapshot restores the cursor position
// so a reconnecting client lands where the user left off.
func TestSnapshotPreservesCursor(t *testing.T) {
	orig := New(20, 5)
	feedStr(orig, "hello\r\nworld") // cursor now at row2 (Y=1), after "world" (X=5)
	wantCur := orig.term.Cursor()

	replay := New(20, 5)
	replay.Feed(orig.Snapshot())
	gotCur := replay.term.Cursor()

	if gotCur.X != wantCur.X || gotCur.Y != wantCur.Y {
		t.Errorf("cursor not restored: got (%d,%d) want (%d,%d)",
			gotCur.X, gotCur.Y, wantCur.X, wantCur.Y)
	}
}

// TestScrollbackCapture feeds more lines than the grid is tall and asserts the
// overflow lines land in scrollback, oldest first, and are replayable.
func TestScrollbackCapture(t *testing.T) {
	const rows = 4
	s := New(20, rows)

	// Print 10 numbered lines. Grid holds only the last `rows`; the first
	// (10 - rows) must have scrolled into scrollback.
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString("L")
		sb.WriteByte(byte('0' + i%10))
		if i < 10 {
			sb.WriteString("\r\n")
		}
	}
	s.Feed([]byte(sb.String()))

	chunks := s.ScrollbackChunks(100)
	if len(chunks) == 0 {
		t.Fatal("expected scrollback after overflowing the grid, got none")
	}

	// Reassemble the captured history as plain text by stripping the trailing
	// CRLF; chunks are oldest-first.
	var hist []string
	for _, c := range chunks {
		hist = append(hist, strings.TrimRight(string(c), "\r\n"))
	}
	// The earliest captured line should be "L1" (first to scroll off).
	if hist[0] != "L1" {
		t.Errorf("oldest scrollback line = %q, want %q", hist[0], "L1")
	}

	// Visible grid should hold the final `rows` lines: L7..L0(=L10).
	visible := s.VisibleText()
	wantVisible := "L7\nL8\nL9\nL0"
	if visible != wantVisible {
		t.Errorf("visible grid = %q, want %q", visible, wantVisible)
	}

	// Combined, scrollback + visible should cover all 10 lines in order.
	combined := append(hist, strings.Split(visible, "\n")...)
	wantSeq := []string{"L1", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9", "L0"}
	if len(combined) != len(wantSeq) {
		t.Fatalf("combined history+visible = %v (len %d), want len %d", combined, len(combined), len(wantSeq))
	}
	for i := range wantSeq {
		if combined[i] != wantSeq[i] {
			t.Errorf("line %d = %q, want %q", i, combined[i], wantSeq[i])
		}
	}
}

// TestScrollbackBounded asserts the ring never exceeds maxScrollback lines.
func TestScrollbackBounded(t *testing.T) {
	s := New(10, 4)
	// Emit far more lines than the cap.
	var sb strings.Builder
	total := maxScrollback + 500
	for i := 0; i < total; i++ {
		sb.WriteString("x\r\n")
	}
	s.Feed([]byte(sb.String()))

	if got := len(s.scrollback); got > maxScrollback {
		t.Errorf("scrollback length %d exceeds cap %d", got, maxScrollback)
	}
	// ScrollbackChunks(n) must also honour the requested limit.
	if chunks := s.ScrollbackChunks(50); len(chunks) > 50 {
		t.Errorf("ScrollbackChunks(50) returned %d chunks, want <= 50", len(chunks))
	}
}

// TestScrollbackChunksLimit checks the n parameter returns the most recent n.
func TestScrollbackChunksLimit(t *testing.T) {
	s := New(20, 3)
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		sb.WriteString("row")
		// encode the index so we can verify recency
		sb.WriteByte(byte('A' + (i-1)%26))
		sb.WriteString("\r\n")
	}
	s.Feed([]byte(sb.String()))

	all := s.ScrollbackChunks(1000)
	if len(all) == 0 {
		t.Fatal("expected scrollback")
	}
	last3 := s.ScrollbackChunks(3)
	if len(last3) != 3 {
		t.Fatalf("ScrollbackChunks(3) returned %d, want 3", len(last3))
	}
	// last3 must equal the tail of all.
	for i := 0; i < 3; i++ {
		if string(last3[i]) != string(all[len(all)-3+i]) {
			t.Errorf("ScrollbackChunks(3)[%d] = %q, want %q", i, last3[i], all[len(all)-3+i])
		}
	}
}

// TestAlternateScreenNoScrollback verifies that switching to the alternate
// screen (full-screen TUI) and scrolling there does not pollute scrollback,
// and that ScrollbackChunks returns nil while on the alt screen.
func TestAlternateScreenNoScrollback(t *testing.T) {
	s := New(20, 4)
	feedStr(s, "primary line\r\n")

	// Enter alternate screen, dump many lines.
	feedStr(s, "\x1b[?1049h")
	var sb strings.Builder
	for i := 0; i < 30; i++ {
		sb.WriteString("alt\r\n")
	}
	s.Feed([]byte(sb.String()))

	if !s.onAlternate() {
		t.Fatal("expected to be on alternate screen")
	}
	if chunks := s.ScrollbackChunks(100); chunks != nil {
		t.Errorf("ScrollbackChunks on alt screen should be nil, got %d chunks", len(chunks))
	}

	// Snapshot on the alt screen should include the alt-screen enter sequence.
	snap := string(s.Snapshot())
	if !strings.Contains(snap, "\x1b[?1049h") {
		t.Errorf("alt-screen snapshot missing enter sequence: %q", snap)
	}
}

// TestResize keeps the emulator usable across a resize.
func TestResize(t *testing.T) {
	s := New(20, 4)
	feedStr(s, "before resize")
	s.Resize(40, 6)
	c, r := s.term.Size()
	if c != 40 || r != 6 {
		t.Fatalf("after Resize size = (%d,%d), want (40,6)", c, r)
	}
	// Should still accept input and reflect it.
	feedStr(s, "\r\nafter")
	if !strings.Contains(s.VisibleText(), "after") {
		t.Errorf("post-resize text missing: %q", s.VisibleText())
	}
}

// TestEmptyFeed is a no-op guard.
func TestEmptyFeed(t *testing.T) {
	s := New(10, 3)
	s.Feed(nil)
	s.Feed([]byte{})
	if s.VisibleText() != "" {
		t.Errorf("empty screen VisibleText = %q, want empty", s.VisibleText())
	}
	if s.Snapshot() == nil {
		t.Error("Snapshot of blank screen should still be non-nil (redraw clears client)")
	}
}

// TestSnapshotRoundTripWithScrollback exercises the full reconnect payload:
// ScrollbackChunks followed by Snapshot, replayed into a fresh terminal, should
// reproduce the combined history + visible text.
func TestSnapshotRoundTripWithScrollback(t *testing.T) {
	const rows = 4
	orig := New(25, rows)
	var sb strings.Builder
	for i := 1; i <= 8; i++ {
		sb.WriteString("content-")
		sb.WriteByte(byte('0' + i))
		if i < 8 {
			sb.WriteString("\r\n")
		}
	}
	orig.Feed([]byte(sb.String()))

	// What a reconnecting client receives: scrollback chunks then the snapshot.
	chunks := orig.ScrollbackChunks(200)
	snap := orig.Snapshot()

	replay := New(25, rows)
	for _, c := range chunks {
		replay.Feed(c)
	}
	replay.Feed(snap)

	// The replayed terminal's visible grid must match the original's.
	if got, want := replay.VisibleText(), orig.VisibleText(); got != want {
		t.Errorf("replayed visible text mismatch\n got: %q\nwant: %q", got, want)
	}
	// And the replayed terminal must itself have accumulated the same scrollback
	// (the chunks scrolled its own grid as they were fed line-by-line).
	if len(replay.scrollback) == 0 {
		t.Error("expected replay to accumulate scrollback from fed history chunks")
	}
}
