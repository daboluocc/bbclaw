package extract

import (
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

func TestParseToolStep(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantName string
		wantHint string
	}{
		{"bash", "⏺ Bash(curl -s https://api.ipify.org)", true, "Bash", "curl -s https://api.ipify.org"},
		{"edit", "⏺ Edit(src/main.go)", true, "Edit", "src/main.go"},
		{"indented continuation marker", "  ⏺ Read(main.go)", true, "Read", "main.go"},
		{"truncated no close paren", "⏺ Bash(for d in ~/gitee ~/github ~/Gitee", true, "Bash", "for d in ~/gitee ~/github ~/Gitee"},

		{"cjk prose", "⏺ 你好你好,我在呢!", false, "", ""},
		{"english prose", "⏺ The capital of France is Paris.", false, "", ""},
		{"prose no paren", "⏺ I'll run the command now", false, "", ""},
		{"prose with spaced paren", "⏺ Paris (the capital) is lovely.", false, "", ""},
		{"chrome model status", "⏺ [Opus 4.8 (1M context)] │ workspace", false, "", ""},
		{"paren at start", "⏺ (earlier) 2 + 2 = 4.", false, "", ""},
		{"result body line", "  ⎿  +7 lines (ctrl+o to expand)", false, "", ""},
		{"blank hint", "⏺ Bash()", false, "", ""},
		{"no marker", "Bash(ls)", false, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := parseToolStep(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("parseToolStep(%q) ok=%v, want %v (got %+v)", tc.line, ok, tc.wantOK, st)
			}
			if ok && (st.Name != tc.wantName || st.Hint != tc.wantHint) {
				t.Errorf("parseToolStep(%q) = {%q,%q}, want {%q,%q}", tc.line, st.Name, st.Hint, tc.wantName, tc.wantHint)
			}
		})
	}
}

func TestScanToolStepsPreservesOrder(t *testing.T) {
	visible := "⏺ Read(a.go)\n  ⎿  120 lines\n⏺ Bash(go build ./...)\n⏺ 你好,做完了。\n❯ \n"
	steps := ScanToolSteps(visible)
	if len(steps) != 2 {
		t.Fatalf("want 2 tool steps, got %d: %+v", len(steps), steps)
	}
	if steps[0].Name != "Read" || steps[1].Name != "Bash" {
		t.Errorf("order/names wrong: %+v", steps)
	}
}

func TestToolHintRuneSafeTruncation(t *testing.T) {
	hint := strings.Repeat("字", 100) // 100 CJK runes (300 bytes)
	st, ok := parseToolStep("⏺ Bash(" + hint + ")")
	if !ok {
		t.Fatal("expected a tool step")
	}
	r := []rune(st.Hint)
	if len(r) > toolHintMax+1 { // +1 for the appended ellipsis
		t.Errorf("hint not truncated: %d runes", len(r))
	}
	// Truncation must be rune-safe: every rune valid (no split multibyte).
	for _, c := range st.Hint {
		if c == '�' {
			t.Fatalf("truncation split a multibyte rune: %q", st.Hint)
		}
	}
}

// Regression: when the LAST "⏺" on screen is a TOOL step, the spoken reply must be
// the EARLIER prose block, not the tool text — otherwise the device would speak
// "Bash df -h" instead of the answer.
func TestExtractMarkerBlockSkipsTrailingToolStep(t *testing.T) {
	s := vtscreen.New(80, 24)
	ext := New(s)
	s.Feed([]byte("\x1b[1;1H" +
		"⏺ 好的,我帮你看磁盘。\r\n" + // prose reply (earlier)
		"⏺ Bash(df -h)\r\n" + // tool step (later, last ⏺)
		"  ⎿  Running…\r\n" +
		"❯ \r\n"))
	r, _ := ext.OnOutput()
	if !strings.Contains(r.Text, "好的,我帮你看磁盘") {
		t.Errorf("expected the prose reply, got %q", r.Text)
	}
	if strings.Contains(r.Text, "Bash") || strings.Contains(r.Text, "df -h") {
		t.Errorf("tool step leaked into the spoken reply: %q", r.Text)
	}
}
