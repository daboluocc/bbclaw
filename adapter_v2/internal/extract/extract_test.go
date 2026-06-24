package extract

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// fixtureCols/Rows must match the geometry the regen script authored the
// recording for (testdata/gen_claude_fixture.go).
const (
	fixtureCols = 80
	fixtureRows = 24
)

// baselineMarker is the inert OSC the regen script embeds at the point where the
// extractor's baseline should be taken: everything before it is pre-existing
// screen state (the user's prompt, an earlier turn), everything after it is the
// newest reply streaming in. vtscreen ignores the OSC, so splitting the raw
// bytes on it lets the test reproduce the real call sequence (Feed prefix →
// New(screen) → Feed suffix → OnOutput).
var baselineMarker = []byte("\x1b]1337;baseline\x07")

// loadFixture reads the recorded claude TUI byte stream and splits it at the
// baseline marker.
func loadFixture(t *testing.T) (pre, post []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_reply.vt"))
	if err != nil {
		t.Fatalf("read fixture: %v (regenerate with: go run testdata/gen_claude_fixture.go)", err)
	}
	idx := bytes.Index(raw, baselineMarker)
	if idx < 0 {
		t.Fatalf("baseline marker not found in fixture; regenerate it")
	}
	return raw[:idx], raw[idx+len(baselineMarker):]
}

// normalize makes the trailing-whitespace tolerance from the acceptance
// criteria explicit: trailing whitespace per line is ignored, as is a trailing
// newline on the whole blob.
func normalize(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// TestExtractFixtureMatchesAnnotation is the core acceptance test: feed the
// recorded claude TUI stream and assert the extracted reply equals the
// human-annotated expectation (trailing-whitespace tolerant).
func TestExtractFixtureMatchesAnnotation(t *testing.T) {
	pre, post := loadFixture(t)

	wantBytes, err := os.ReadFile(filepath.Join("testdata", "claude_reply.expected.txt"))
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	want := normalize(string(wantBytes))

	s := vtscreen.New(fixtureCols, fixtureRows)
	s.Feed(pre)  // prompt + earlier turn now on screen
	e := New(s)  // baseline captured here
	s.Feed(post) // newest reply streams in (interleaved with spinner)
	reply, changed := e.OnOutput()

	if !changed {
		t.Fatalf("expected a reply after feeding the new turn, got changed=false (text=%q)", reply.Text)
	}
	if reply.Complete {
		t.Errorf("extract must not set Complete (that is boundary.go's job); got Complete=true")
	}
	if got := normalize(reply.Text); got != want {
		t.Errorf("extracted reply mismatch\n got: %q\nwant: %q", got, want)
	}

	// The user's prompt text and the spinner status must not leak into the reply.
	if strings.Contains(reply.Text, "capital of France?") {
		t.Errorf("reply leaked the user's input prompt: %q", reply.Text)
	}
	if strings.Contains(reply.Text, "esc to interrupt") || strings.Contains(reply.Text, "Cogitating") {
		t.Errorf("reply leaked the spinner status line: %q", reply.Text)
	}
	// The earlier turn (baseline) must be excluded.
	if strings.Contains(reply.Text, "2 + 2") {
		t.Errorf("reply leaked the earlier (baseline) turn: %q", reply.Text)
	}
}

// TestSpinnerRedrawsNoJitter exercises the acceptance criterion "对 spinner/重绘
// 帧不产生重复/抖动文本": after the reply has been emitted once, feeding many
// more spinner redraw frames must not change the extracted text nor report a new
// emission.
func TestSpinnerRedrawsNoJitter(t *testing.T) {
	pre, post := loadFixture(t)

	s := vtscreen.New(fixtureCols, fixtureRows)
	s.Feed(pre)
	e := New(s)
	s.Feed(post)

	first, changed := e.OnOutput()
	if !changed {
		t.Fatalf("expected first emission to report changed=true")
	}

	// Drive 50 spinner redraws of the working status line, in place, exactly as a
	// busy claude session would, calling OnOutput after each. None may change the
	// extracted reply.
	spin := []string{"✶", "✻", "✽", "✻"}
	for i := 0; i < 50; i++ {
		glyph := spin[i%len(spin)]
		frame := "\x1b[21;1H\r\x1b[2K\x1b[2m" +
			glyph + " Cogitating… (" + itoa(10+i) + "s · ↑ " + itoa(600+i*17) + " tokens · esc to interrupt)" +
			"\x1b[0m"
		s.Feed([]byte(frame))
		reply, ch := e.OnOutput()
		if ch {
			t.Fatalf("spinner redraw %d produced a new emission: %q", i, reply.Text)
		}
		if reply.Text != first.Text {
			t.Fatalf("spinner redraw %d jittered the reply text\n got: %q\nwant: %q", i, reply.Text, first.Text)
		}
	}
}

// TestStreamingMonotonic feeds the reply chunk-by-chunk and asserts the
// extracted text only ever grows toward the final reply — never dropping a line
// it already surfaced and never duplicating one — and that intervening
// spinner-only frames report changed=false.
func TestStreamingMonotonic(t *testing.T) {
	s := vtscreen.New(fixtureCols, fixtureRows)
	// Establish a prompt + empty baseline.
	s.Feed([]byte("\x1b[2J\x1b[H"))
	e := New(s)

	steps := []struct {
		feed        string
		wantChanged bool
		wantSubstr  string // must be present in the extracted reply after this step
	}{
		{feed: "\x1b[21;1H\r\x1b[2K✶ Working… (1s · esc to interrupt)", wantChanged: false},
		{feed: "\x1b[3;1H⏺ First line of the reply.", wantChanged: true, wantSubstr: "First line of the reply."},
		{feed: "\x1b[21;1H\r\x1b[2K✻ Working… (2s · esc to interrupt)", wantChanged: false},
		{feed: "\x1b[4;1H  Second line continues.", wantChanged: true, wantSubstr: "Second line continues."},
		{feed: "\x1b[21;1H\r\x1b[2K✽ Working… (3s · esc to interrupt)", wantChanged: false},
	}

	var lastText string
	var lineCount int
	for i, st := range steps {
		s.Feed([]byte(st.feed))
		reply, changed := e.OnOutput()
		if changed != st.wantChanged {
			t.Fatalf("step %d: changed=%v want %v (text=%q)", i, changed, st.wantChanged, reply.Text)
		}
		if st.wantSubstr != "" && !strings.Contains(reply.Text, st.wantSubstr) {
			t.Fatalf("step %d: reply %q missing %q", i, reply.Text, st.wantSubstr)
		}
		// Monotonic growth: never lose earlier text, never shrink line count.
		if changed {
			if lastText != "" && !strings.Contains(reply.Text, "First line of the reply.") {
				t.Fatalf("step %d: reply dropped already-surfaced text: %q", i, reply.Text)
			}
			n := len(strings.Split(reply.Text, "\n"))
			if n < lineCount {
				t.Fatalf("step %d: line count shrank from %d to %d: %q", i, lineCount, n, reply.Text)
			}
			lineCount = n
			lastText = reply.Text
		}
	}

	// No duplicate lines in the final reply.
	final := strings.Split(lastText, "\n")
	seen := map[string]int{}
	for _, l := range final {
		seen[l]++
		if seen[l] > 1 {
			t.Errorf("duplicate line in final reply: %q\nfull: %q", l, lastText)
		}
	}
}

// TestBaselineIsolatesNewestTurn proves the baseline diff: an earlier turn
// already on screen when New() is called is excluded; only post-baseline content
// is surfaced.
func TestBaselineIsolatesNewestTurn(t *testing.T) {
	s := vtscreen.New(40, 10)
	s.Feed([]byte("\x1b[1;1H⏺ Older answer about cats."))
	e := New(s) // baseline includes the older answer

	s.Feed([]byte("\x1b[3;1H⏺ Newer answer about dogs."))
	reply, changed := e.OnOutput()
	if !changed {
		t.Fatal("expected a reply for the new turn")
	}
	if strings.Contains(reply.Text, "cats") {
		t.Errorf("baseline (older) turn leaked into reply: %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "dogs") {
		t.Errorf("newest turn missing from reply: %q", reply.Text)
	}
}

// Regression for the long-list-reply TTS-truncation bug: a reply TALLER than the
// terminal grid scrolls its top — including claude's "⏺" anchor and the opening
// lines — off the visible grid into scrollback. The marker scan used to read the
// visible grid only, so it lost the anchor and surfaced just the visible tail (or
// nothing), and the device spoke a truncated reply while the web terminal (which
// replays scrollback) showed the whole thing. The scan must include scrollback so
// the full reply is recovered. Uses a deliberately tiny grid to force the scroll
// with a short fixture; the geometry, not the line count, is what triggers it.
func TestExtractRecoversReplyTallerThanGrid(t *testing.T) {
	s := vtscreen.New(40, 6) // 6 visible rows: an 8-item list overflows it
	ext := New(s)

	// Stream the reply as real newline-advanced lines (not absolute cursor moves),
	// so feeding past the bottom row genuinely scrolls the top into scrollback —
	// exactly how claude appends a long answer.
	s.Feed([]byte(
		"⏺ 行前清单:\r\n" +
			"1. 精读年报\r\n" +
			"2. 读两篇专访\r\n" +
			"3. 备样章\r\n" +
			"4. 备专利趋势图\r\n" +
			"5. 录三段Demo\r\n" +
			"6. 备一页案例集\r\n" +
			"7. 打印目录样章\r\n" +
			"8. 备安全合规页\r\n"))

	r, _ := ext.OnOutput()
	// The header (scrolled off first) AND every item — front, middle, and tail —
	// must all survive, proving nothing was lost to the grid height.
	for _, want := range []string{
		"行前清单", "1. 精读年报", "2. 读两篇专访", "4. 备专利趋势图", "8. 备安全合规页",
	} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("tall reply truncated — missing %q\nfull reply: %q", want, r.Text)
		}
	}
}

// itoa is a tiny dependency-free int→string for building spinner frames.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Regression for the SaaS voice bug: on the first turn of a fresh claude session
// the screen is the boxed welcome banner (no "⏺" reply yet). The diff fallback
// used to return that whole banner as the "reply", so the device spoke the
// startup screen. isClaudeScreen must suppress it — empty until "⏺" appears —
// while a genuinely marker-less CLI still gets the diff fallback.
func TestExtractSuppressesClaudeChromeUntilMarker(t *testing.T) {
	s := vtscreen.New(80, 24)
	ext := New(s)

	// claude welcome/idle chrome: boxed banner + "❯" prompt, no "⏺" reply.
	s.Feed([]byte("\x1b[1;1H╭─── Claude Code v2.1.185 ──────────────────╮\r\n" +
		"│            Welcome back mikas!             │\r\n" +
		"╰────────────────────────────────────────────╯\r\n" +
		"❯ Try \"edit main.go\"\r\n"))
	if r, _ := ext.OnOutput(); strings.TrimSpace(r.Text) != "" {
		t.Errorf("welcome chrome leaked as reply: %q", r.Text)
	}

	// claude emits its reply — now the "⏺" block is the reply.
	s.Feed([]byte("\x1b[6;1H⏺ The answer is 42.\r\n"))
	if r, _ := ext.OnOutput(); !strings.Contains(r.Text, "The answer is 42.") {
		t.Errorf("reply not extracted once marker present: %q", r.Text)
	}
}

// Regression for the on-device SaaS bug (TTS spoke "[Opus 4.8 (1M context)] │
// workspace" / "⏵⏵ bypass permissions on …"): on --resume claude re-renders its
// model/usage status as "⏺ <chrome>" blocks AFTER the real reply. The last-"⏺"
// scan anchored on those, so the device spoke claude's status footer. The marker
// scan must skip "⏺ <noise>" lines and anchor on the last REAL reply.
func TestExtractSkipsResumeStatusMarkerBlocks(t *testing.T) {
	s := vtscreen.New(80, 24)
	ext := New(s)
	s.Feed([]byte("\x1b[1;1H" +
		"⏺ 好的,我在。\r\n" + // the REAL reply
		"\r\n" +
		"⏺ [Opus 4.8 (1M context)] │ workspace\r\n" + // resume status chrome (a later "⏺")
		"  Context ░░░░░░░░░░ 3% │ Usage █░░░░░░░░░ 9% (resets in 4h 20m)\r\n" +
		"⏺ ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents\r\n" + // bypass footer as "⏺"
		"❯ \r\n"))
	r, _ := ext.OnOutput()
	if !strings.Contains(r.Text, "好的,我在。") {
		t.Errorf("real reply not extracted: %q", r.Text)
	}
	if strings.Contains(r.Text, "Opus 4.8") || strings.Contains(r.Text, "bypass permissions") || strings.Contains(r.Text, "context)") {
		t.Errorf("resume status chrome leaked into reply: %q", r.Text)
	}
}

// And when the ONLY "⏺" lines are status chrome (no real reply on screen yet —
// e.g. right after --resume, before the new turn's reply renders), nothing is
// extracted, rather than speaking the chrome.
func TestExtractStatusOnlyMarkersYieldNothing(t *testing.T) {
	s := vtscreen.New(80, 24)
	ext := New(s)
	s.Feed([]byte("\x1b[1;1H" +
		"⏺ [Opus 4.8 (1M context)] │ workspace\r\n" +
		"  Context ░░░░░░░░░░ 3% │ Usage █░░░░░░░░░ 9%\r\n" +
		"❯ \r\n"))
	if r, _ := ext.OnOutput(); strings.TrimSpace(r.Text) != "" {
		t.Errorf("status-only chrome leaked as reply: %q", r.Text)
	}
}

func TestExtractFallbackStillWorksForMarkerlessCLI(t *testing.T) {
	s := vtscreen.New(80, 24)
	ext := New(s)
	// A plain marker-less CLI: no box-drawing, no "❯", no "⏺" — diff fallback applies.
	s.Feed([]byte("\x1b[1;1Hplain assistant reply line\r\n"))
	if r, _ := ext.OnOutput(); !strings.Contains(r.Text, "plain assistant reply line") {
		t.Errorf("marker-less diff fallback regressed: %q", r.Text)
	}
}

// Regression: claude v2 lays out a multi-paragraph reply's later paragraphs at
// column 0 (flush-left), not as 2-space-indented continuations. The extractor
// must capture the WHOLE reply up to the "✻ … for Ns" completion summary, not
// truncate at the first flush-left paragraph (the on-device bug where only
// "你好你好,我在呢!" was spoken and the follow-up paragraph was dropped).
func TestExtractMarkerBlockFlushLeftParagraphs(t *testing.T) {
	s := vtscreen.New(80, 24)
	ext := New(s)
	s.Feed([]byte("\x1b[1;1H" +
		"⏺ 你好你好,我在呢!\r\n" +
		"\r\n" +
		"对了,咱们还没正式认识——我该怎么称呼你?\r\n" + // flush-left 2nd paragraph
		"✻ Worked for 2s\r\n" + // completion summary → reply ends here
		"❯ \r\n"))
	r, _ := ext.OnOutput()
	if !strings.Contains(r.Text, "你好你好,我在呢!") || !strings.Contains(r.Text, "对了,咱们还没正式认识") {
		t.Errorf("flush-left 2nd paragraph dropped: %q", r.Text)
	}
	if strings.Contains(r.Text, "Worked for") || strings.Contains(r.Text, "❯") {
		t.Errorf("reply leaked the completion summary / prompt: %q", r.Text)
	}
}
