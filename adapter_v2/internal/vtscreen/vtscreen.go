// Package vtscreen is a server-side virtual terminal emulator. Every byte read
// from the PTY is Fed here so the server always holds the exact screen grid and
// scrollback — independent of whether any client is connected. This is the
// foundation for two features:
//
//   - reconnect recovery: a reconnecting terminal client gets a Snapshot
//     instead of a blank screen (see session + termchan).
//   - text extraction: the device channel scrapes the grid for the assistant's
//     reply text (see package extract).
//
// Compare: dinotty src/vt_screen.rs.
//
// # VT engine choice
//
// We embed github.com/hinshun/vt10x as the ANSI/VT100 parser + grid model. It
// is the most widely used pure-Go terminal emulator (it backs ActiveState's
// `expect` test harness and several TUI test frameworks), parses the same
// xterm/st sequence set the agent CLIs emit, and exposes a clean read-only
// View (Cell/Cursor/Size/Mode). Alternatives considered:
//
//   - Porting dinotty's hand-rolled `vte`-based performer to Go: a lot of
//     fragile escape-handling code to re-own and test ourselves.
//   - github.com/charmbracelet/x/exp/teatest's emulator: not a standalone,
//     stable public API.
//
// vt10x has one gap for our use: it does NOT retain scrollback — lines that
// scroll off the top of the primary screen are discarded. The dinotty
// reference keeps a scrollback ring, which we need for reconnect history. We
// reconstruct scrollback ourselves at this layer (see scrollCapture) by
// diffing the grid before/after each Feed to recover the rows that scrolled
// off, then pushing them into a bounded ring.
package vtscreen

import (
	"strconv"
	"strings"

	"github.com/hinshun/vt10x"
)

// maxScrollback bounds the history ring. ~1000 lines is plenty for reconnect
// recovery without letting a chatty CLI grow memory without limit (matches the
// budget called out in the issue; dinotty uses a larger 10k ring).
const maxScrollback = 1000

// vt10x attribute bits (mirrored from the library's unexported constants in
// state.go). Glyph.Mode is a bitfield; we read these to re-encode SGR in
// Snapshot so a replayed screen keeps bold/underline/etc.
const (
	attrReverse   = 1 << 0
	attrUnderline = 1 << 1
	attrBold      = 1 << 2
	attrGfx       = 1 << 3 // alternate (line-drawing) charset — already mapped to unicode by vt10x
	attrItalic    = 1 << 4
	attrBlink     = 1 << 5
)

// vt10x color sentinels. Colors [0,16) are ANSI, [16,256) are xterm-256,
// [256, 1<<24) are packed 24-bit RGB (r<<16|g<<8|b), and >= 1<<24 are the
// "default" fg/bg/cursor markers.
const (
	colorDefaultBase = 1 << 24 // DefaultFG; DefaultBG/Cursor are the next two
	colorRGBBase     = 1 << 8  // values >= 256 and < 1<<24 are RGB triples
)

// Screen maintains a terminal grid plus a bounded scrollback ring. It is NOT
// goroutine-safe; the owning Session serialises access behind a mutex.
type Screen struct {
	term vt10x.Terminal // the embedded vt10x parser + grid (current screen)
	cols int
	rows int

	// scrollback holds lines that scrolled off the top of the primary screen,
	// oldest first, capped at maxScrollback. Each entry is one fully-resolved
	// grid row (length == cols at the time it scrolled off).
	scrollback []scrollLine

	// pending holds the trailing bytes of an incomplete UTF-8 sequence carried
	// over from the previous Feed. vt10x does NOT buffer a partial rune across
	// Write calls (a multibyte rune split across writes is dropped), and a PTY
	// read can split a rune at the chunk boundary, so we hold the incomplete
	// tail here and prepend it to the next chunk. ≤3 bytes (the max UTF-8
	// continuation length).
	pending []byte
}

// scrollLine is one captured scrollback row.
type scrollLine []vt10x.Glyph

// New creates a blank screen of the given size.
func New(cols, rows int) *Screen {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return &Screen{
		term: vt10x.New(vt10x.WithSize(cols, rows)),
		cols: cols,
		rows: rows,
	}
}

// Feed advances the emulator by parsing raw PTY output bytes, capturing any
// rows that scroll off the top of the primary screen into the scrollback ring.
//
// The whole chunk is written to vt10x in ONE call (after prepending any partial
// rune held from the previous Feed). This is essential for correctness: vt10x
// does not buffer an incomplete UTF-8 sequence across Write calls, so feeding
// byte-by-byte — or splitting a multibyte rune (❯, …, box-drawing, CJK …) at a
// chunk boundary — silently drops that rune. claude's TUI is dense with
// multibyte glyphs, so a faithful device-line scrape requires whole-rune writes.
//
// vt10x discards lines that scroll off the top, so to recover them for the
// scrollback ring we snapshot the grid before the write and diff it against the
// result, pushing any rows that shifted off. This captures a clean upward scroll
// of one chunk; a single chunk that scrolls more than a screenful (rare for the
// ~KB PTY reads we get) keeps only the most recent screen of evicted rows, which
// is acceptable for best-effort reconnect history.
func (s *Screen) Feed(p []byte) {
	data := p
	if len(s.pending) > 0 {
		data = append(s.pending, p...)
		s.pending = nil
	}

	// Hold back a trailing incomplete UTF-8 sequence for the next Feed.
	n := completeUTF8Prefix(data)
	if n < len(data) {
		s.pending = append([]byte(nil), data[n:]...) // fresh copy: never aliases data
	}
	data = data[:n]
	if len(data) == 0 {
		return
	}

	// Feed the line CONTENT up to each '\n' first, then the '\n' on its own with a
	// before/after diff. A '\n' at the bottom row is what scrolls a line off the
	// top; isolating it means the just-written line is already on screen when we
	// snapshot, so captureScroll's diff cleanly recovers the evicted row. Writing
	// the content first (in bulk) keeps multibyte runes and ANSI escapes intact —
	// '\n' (0x0A) never appears inside either. (A line that scrolls by wrapping,
	// with no '\n', is not captured; that is an acceptable best-effort gap for
	// reconnect history.)
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			if i > start {
				_, _ = s.term.Write(data[start:i]) // line content; no scroll yet
			}
			s.feedNewline(data[i : i+1]) // the '\n' alone, with scroll capture
			start = i + 1
		}
	}
	if start < len(data) {
		_, _ = s.term.Write(data[start:]) // trailing content after the last '\n'
	}
}

// feedNewline writes a single '\n' (the byte that scrolls a line off the bottom),
// capturing the evicted row into the scrollback ring via a before/after diff.
func (s *Screen) feedNewline(nl []byte) {
	before := s.copyGrid()
	// vt10x.Terminal implements io.Writer; Write never errors for an in-memory
	// terminal, so we ignore the result.
	_, _ = s.term.Write(nl)
	s.captureScroll(before)
}

// completeUTF8Prefix returns the length of the longest prefix of b that ends on
// a UTF-8 rune boundary — i.e. it excludes a trailing incomplete multibyte
// sequence (but never an ASCII byte or a complete rune). Escape sequences are
// all single-byte and so are never held back; only a split multibyte rune is.
func completeUTF8Prefix(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	// Walk back over trailing continuation bytes (10xxxxxx) to find the lead.
	i := len(b) - 1
	for i >= 0 && b[i]&0xC0 == 0x80 {
		i--
	}
	if i < 0 {
		return len(b) // all continuation bytes (malformed) — feed as-is
	}
	c := b[i]
	var need int
	switch {
	case c&0x80 == 0x00:
		need = 1
	case c&0xE0 == 0xC0:
		need = 2
	case c&0xF0 == 0xE0:
		need = 3
	case c&0xF8 == 0xF0:
		need = 4
	default:
		return len(b) // invalid lead byte — let vt10x deal with it
	}
	if len(b)-i >= need {
		return len(b) // the final rune is complete
	}
	return i // incomplete final rune starts at i; cut before it
}

// captureScroll compares the grid before the last byte against the grid after
// it and, if the screen scrolled up by k rows, appends the k displaced top rows
// of `before` to the scrollback ring (oldest first).
func (s *Screen) captureScroll(before [][]vt10x.Glyph) {
	if s.onAlternate() {
		return // alt screen has no history
	}
	after := s.copyGrid()
	k := scrollAmount(before, after)
	for i := 0; i < k; i++ {
		s.pushScrollback(before[i])
	}
}

// Size reports the current grid dimensions. Used by termchan to populate the
// reconnected{cols,rows} message so a joining client sizes its xterm.js to match.
func (s *Screen) Size() (cols, rows int) {
	return s.cols, s.rows
}

// Resize changes the grid dimensions. vt10x reflows its own grid; existing
// scrollback is left untouched (history rows keep their original width).
func (s *Screen) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	s.term.Resize(cols, rows)
	s.cols, s.rows = cols, rows
}

// onAlternate reports whether the terminal is currently on the alternate
// screen (e.g. a full-screen TUI pane). Scrollback is only meaningful for the
// primary screen, matching the dinotty reference.
func (s *Screen) onAlternate() bool {
	return s.term.Mode()&vt10x.ModeAltScreen != 0
}

// scrollAmount returns the number of rows the primary screen scrolled up
// between two snapshots taken around a single fed byte: the smallest k in
// [1, rows-1] such that old rows [k, rows) equal new rows [0, rows-k). Returns
// 0 when no genuine shift explains the change (no scroll, or a full repaint).
//
// k is capped at rows-1 (not rows) so the compared band is always non-empty:
// matching an empty band would spuriously report a full-screen scroll for an
// ordinary write on the bottom row. We additionally require the top row to have
// actually changed, since a true scroll always evicts the old top row.
func scrollAmount(old, neu [][]vt10x.Glyph) int {
	rows := len(old)
	if rows < 2 || len(neu) != rows {
		return 0
	}
	if rowsEqual(old[0], neu[0]) {
		return 0 // top row unchanged ⇒ nothing scrolled off
	}
	// Pick the smallest shift that explains the change (k ascending).
	for k := 1; k <= rows-1; k++ {
		if gridSuffixMatchesPrefix(old, neu, k) {
			return k
		}
	}
	return 0
}

// gridSuffixMatchesPrefix reports whether old[k:rows] == neu[0:rows-k] row for
// row. The compared band is old rows [k, rows) against new rows [0, rows-k):
// these are the rows that merely shifted up and so must be identical for a true
// scroll. (The new bottom k rows are the freshly scrolled-in content and are
// not compared.)
func gridSuffixMatchesPrefix(old, neu [][]vt10x.Glyph, k int) bool {
	rows := len(old)
	for i := k; i < rows; i++ {
		if !rowsEqual(old[i], neu[i-k]) {
			return false
		}
	}
	return true
}

func rowsEqual(a, b []vt10x.Glyph) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pushScrollback appends one row to the ring, evicting the oldest line when the
// ring is full.
func (s *Screen) pushScrollback(row []vt10x.Glyph) {
	line := make(scrollLine, len(row))
	copy(line, row)
	s.scrollback = append(s.scrollback, line)
	if len(s.scrollback) > maxScrollback {
		// Drop the oldest. Re-slice onto a fresh backing array periodically to
		// avoid the underlying array growing unbounded from repeated head drops.
		over := len(s.scrollback) - maxScrollback
		s.scrollback = append(s.scrollback[:0:0], s.scrollback[over:]...)
	}
}

// copyGrid returns a deep copy of the current visible grid (rows × cols of
// glyphs). Used both as the scroll-diff baseline and as the source for Snapshot
// / VisibleText so all readers see a consistent grid.
func (s *Screen) copyGrid() [][]vt10x.Glyph {
	grid := make([][]vt10x.Glyph, s.rows)
	for y := 0; y < s.rows; y++ {
		row := make([]vt10x.Glyph, s.cols)
		for x := 0; x < s.cols; x++ {
			row[x] = s.term.Cell(x, y)
		}
		grid[y] = row
	}
	return grid
}

// Snapshot renders the current visible grid as a byte string of ANSI sequences
// that, when written to a fresh xterm.js, reproduces the screen. Sent on
// reconnect so the client redraws to the pre-disconnect state.
//
// Format (mirrors dinotty's snapshot): hide cursor, reset SGR, optionally enter
// the alternate screen, then for each row move to its start, erase it, and emit
// its glyphs with minimal SGR transitions; finally restore the cursor position
// and visibility.
func (s *Screen) Snapshot() []byte {
	var b strings.Builder
	b.Grow(s.cols * s.rows * 2)

	b.WriteString("\x1b[?25l") // hide cursor while we repaint
	b.WriteString("\x1b[0m")   // reset all attributes
	if s.onAlternate() {
		b.WriteString("\x1b[?1049h") // enter alternate screen
	}

	grid := s.copyGrid()
	prev := defaultSGR()
	for y, row := range grid {
		// CUP to (row, col 1) then EL(2) to clear the line before drawing.
		b.WriteString("\x1b[")
		b.WriteString(strconv.Itoa(y + 1))
		b.WriteString(";1H\x1b[2K")

		last := lastContentCol(row)
		for x := 0; x < last; x++ {
			g := row[x]
			cur := sgrOf(g)
			if cur != prev {
				b.WriteString(cur.encode())
				prev = cur
			}
			b.WriteRune(glyphRune(g))
		}
		if !prev.isDefault() {
			b.WriteString("\x1b[0m")
			prev = defaultSGR()
		}
	}

	b.WriteString("\x1b[0m")

	// Restore cursor position (1-based) and visibility.
	cur := s.term.Cursor()
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(cur.Y + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(cur.X + 1))
	b.WriteByte('H')
	if s.term.CursorVisible() {
		b.WriteString("\x1b[?25h")
	}

	return []byte(b.String())
}

// ScrollbackChunks returns up to the last n scrollback lines as replayable
// chunks (one []byte per line, each ending in CRLF), oldest first. Sent before
// Snapshot on reconnect to restore history. Returns nil when on the alternate
// screen (which has no history) or when there is no scrollback.
func (s *Screen) ScrollbackChunks(n int) [][]byte {
	if n <= 0 || s.onAlternate() || len(s.scrollback) == 0 {
		return nil
	}
	start := 0
	if len(s.scrollback) > n {
		start = len(s.scrollback) - n
	}
	out := make([][]byte, 0, len(s.scrollback)-start)
	for _, row := range s.scrollback[start:] {
		out = append(out, renderRowANSI(row))
	}
	return out
}

// renderRowANSI encodes one scrollback row as replayable ANSI ending in CRLF,
// with minimal SGR transitions and a trailing reset if any attributes were set.
func renderRowANSI(row []vt10x.Glyph) []byte {
	var b strings.Builder
	prev := defaultSGR()
	last := lastContentCol(row)
	for x := 0; x < last; x++ {
		g := row[x]
		cur := sgrOf(g)
		if cur != prev {
			b.WriteString(cur.encode())
			prev = cur
		}
		b.WriteRune(glyphRune(g))
	}
	if !prev.isDefault() {
		b.WriteString("\x1b[0m")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// VisibleText returns the plain-text content of the visible grid (no ANSI),
// trailing blank columns trimmed per row and trailing blank rows trimmed,
// rows joined by '\n'. Used by package extract to scrape the assistant's reply.
func (s *Screen) VisibleText() string {
	grid := s.copyGrid()
	lines := make([]string, s.rows)
	for y, row := range grid {
		last := lastContentColPlain(row)
		var sb strings.Builder
		for x := 0; x < last; x++ {
			sb.WriteRune(glyphRune(row[x]))
		}
		lines[y] = sb.String()
	}
	// Trim trailing all-blank rows so extract doesn't see a wall of empties.
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	return strings.Join(lines[:end], "\n")
}

// ScrollbackText returns up to the last n scrollback rows as plain text (no
// ANSI), oldest first, one row per line, trailing blank columns trimmed per row
// — the scrollback counterpart of VisibleText. Package extract joins this ahead
// of VisibleText so a reply TALLER than the visible grid (whose top, including
// claude's "⏺" anchor, has scrolled off) is still recovered whole instead of
// truncated to the visible tail. Returns "" on the alternate screen (which keeps
// no history) or when the scrollback ring is empty.
func (s *Screen) ScrollbackText(n int) string {
	if n <= 0 || s.onAlternate() || len(s.scrollback) == 0 {
		return ""
	}
	start := 0
	if len(s.scrollback) > n {
		start = len(s.scrollback) - n
	}
	rows := s.scrollback[start:]
	lines := make([]string, len(rows))
	for i, row := range rows {
		last := lastContentColPlain(row)
		var sb strings.Builder
		for x := 0; x < last; x++ {
			sb.WriteRune(glyphRune(row[x]))
		}
		lines[i] = sb.String()
	}
	return strings.Join(lines, "\n")
}

// glyphRune resolves a glyph to its display rune, mapping NUL/zero (a never-set
// cell, or the trailing half of a wide char) to a space.
func glyphRune(g vt10x.Glyph) rune {
	if g.Char == 0 {
		return ' '
	}
	return g.Char
}

// lastContentCol returns the column index one past the last cell that carries
// visible content OR a non-default attribute (so a trailing run of styled
// spaces — e.g. a highlighted selection bar — is preserved on replay).
func lastContentCol(row []vt10x.Glyph) int {
	for x := len(row) - 1; x >= 0; x-- {
		g := row[x]
		if (g.Char != ' ' && g.Char != 0) || !sgrOf(g).isDefault() {
			return x + 1
		}
	}
	return 0
}

// lastContentColPlain is lastContentCol's plain-text counterpart: it ignores
// attributes and trims on character content only.
func lastContentColPlain(row []vt10x.Glyph) int {
	for x := len(row) - 1; x >= 0; x-- {
		if c := row[x].Char; c != ' ' && c != 0 {
			return x + 1
		}
	}
	return 0
}

// sgrState is the subset of glyph attributes we serialise on snapshot/replay.
//
// Note its zero value is NOT the default style: a default cell carries
// vt10x.DefaultFG/DefaultBG colours (which are large sentinels, not 0). Use
// defaultSGR() / isDefault() rather than comparing against sgrState{}.
type sgrState struct {
	bold, dim, italic, underline, inverse bool
	fg, bg                                vt10x.Color
}

// defaultSGR is the style of an untouched cell: no attributes, default colours.
func defaultSGR() sgrState {
	return sgrState{fg: vt10x.DefaultFG, bg: vt10x.DefaultBG}
}

// isDefault reports whether this state needs no SGR (it equals an SGR reset).
func (s sgrState) isDefault() bool { return s == defaultSGR() }

// sgrOf extracts the renderable attributes from a glyph. Note vt10x already
// folds reverse-video into swapped fg/bg at setChar time, but it also keeps the
// attrReverse bit set; we surface it so the inverse intent is preserved.
func sgrOf(g vt10x.Glyph) sgrState {
	st := sgrState{fg: g.FG, bg: g.BG}
	if g.Mode&attrBold != 0 {
		st.bold = true
	}
	if g.Mode&attrItalic != 0 {
		st.italic = true
	}
	if g.Mode&attrUnderline != 0 {
		st.underline = true
	}
	if g.Mode&attrReverse != 0 {
		st.inverse = true
	}
	if g.Mode&attrBlink != 0 {
		// We don't render blink, but treat it as "non-default" so the cell's
		// styling boundary is still tracked. No SGR emitted for it.
	}
	return st
}

// encode renders the SGR escape that establishes exactly this state from a
// reset baseline (always leads with "0" so it is self-contained).
func (s sgrState) encode() string {
	params := make([]string, 0, 6)
	params = append(params, "0")
	if s.bold {
		params = append(params, "1")
	}
	if s.dim {
		params = append(params, "2")
	}
	if s.italic {
		params = append(params, "3")
	}
	if s.underline {
		params = append(params, "4")
	}
	if s.inverse {
		params = append(params, "7")
	}
	if p := encodeColor(s.fg, true); p != "" {
		params = append(params, p)
	}
	if p := encodeColor(s.bg, false); p != "" {
		params = append(params, p)
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

// encodeColor renders a color as SGR parameter(s), or "" for the default color.
// fg selects the foreground (30s/90s/38) vs background (40s/100s/48) family.
func encodeColor(c vt10x.Color, fg bool) string {
	if c >= colorDefaultBase {
		return "" // DefaultFG/DefaultBG → no explicit color param
	}
	switch {
	case c < 8: // standard ANSI
		base := 30
		if !fg {
			base = 40
		}
		return strconv.Itoa(base + int(c))
	case c < 16: // bright ANSI
		base := 90
		if !fg {
			base = 100
		}
		return strconv.Itoa(base + int(c) - 8)
	case c < colorRGBBase: // xterm-256 index
		sel := "38;5;"
		if !fg {
			sel = "48;5;"
		}
		return sel + strconv.Itoa(int(c))
	default: // packed 24-bit RGB (r<<16 | g<<8 | b)
		r := (c >> 16) & 0xff
		g := (c >> 8) & 0xff
		b := c & 0xff
		sel := "38;2;"
		if !fg {
			sel = "48;2;"
		}
		return sel + strconv.Itoa(int(r)) + ";" + strconv.Itoa(int(g)) + ";" + strconv.Itoa(int(b))
	}
}
