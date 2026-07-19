package tasklist

import "testing"

func TestScanTaskListCandidates_BallotBoxes(t *testing.T) {
	// The motivating screenshot: indented "☐ …" rows under a TodoWrite tool line.
	visible := "" +
		"⏺ Update Todos\n" +
		"  ☐ 精读年报\n" +
		"  ☐ 读孙锋峰专访\n" +
		"  ☒ 准备预研样章\n" +
		"❯ "
	got := ScanTaskListCandidates(visible)
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d: %+v", len(got), got)
	}
	if got[0].Lead != "U+2610" || got[0].Text != "精读年报" {
		t.Errorf("row0 = %+v, want U+2610 / 精读年报", got[0])
	}
	if got[2].Lead != "U+2612" {
		t.Errorf("row2 lead = %s, want U+2612 (☒)", got[2].Lead)
	}
	if got[0].Indent != 2 {
		t.Errorf("row0 indent = %d, want 2", got[0].Indent)
	}
}

func TestScanTaskListCandidates_ASCIICheckbox(t *testing.T) {
	visible := "  [ ] first\n  [x] second\n  [-] third"
	got := ScanTaskListCandidates(visible)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(got), got)
	}
	if got[0].Lead != "[ ]" || got[0].Text != "first" {
		t.Errorf("row0 = %+v, want [ ] / first", got[0])
	}
	if got[1].Lead != "[x]" {
		t.Errorf("row1 lead = %s, want [x]", got[1].Lead)
	}
}

func TestScanTaskListCandidates_RejectsProseAndChrome(t *testing.T) {
	cases := map[string]string{
		"prose":           "⏺ The capital of France is Paris.",
		"markdown dash":   "- item one\n- item two", // plain bullets are not checklist glyphs
		"permission menu": "❯ 1. Yes\n  2. No",      // digits, not checkboxes
		"bare glyph":      "  ☐ ",                   // glyph with no text
		"triangle arrow":  "▶ selected\n◀ back",     // intentionally excluded
	}
	for name, visible := range cases {
		if got := ScanTaskListCandidates(visible); len(got) != 0 {
			t.Errorf("%s: want 0 candidates, got %d: %+v", name, len(got), got)
		}
	}
}

func TestLongestRun_FiltersStrayRow(t *testing.T) {
	// A real 3-row block (indent 2) plus one stray checkbox at a different indent;
	// LongestRun should return only the contiguous same-indent block.
	cands := []TaskCandidate{
		{Lead: "U+2610", Text: "a", Indent: 2},
		{Lead: "U+2610", Text: "b", Indent: 2},
		{Lead: "U+2612", Text: "c", Indent: 2},
		{Lead: "U+25A1", Text: "stray", Indent: 6},
	}
	run := LongestRun(cands)
	if len(run) != 3 {
		t.Fatalf("want run of 3, got %d: %+v", len(run), run)
	}
	if run[0].Text != "a" || run[2].Text != "c" {
		t.Errorf("run = %+v, want a..c", run)
	}
}

func TestLongestRun_BelowThreshold(t *testing.T) {
	// A single candidate is not a block.
	if run := LongestRun([]TaskCandidate{{Lead: "U+2610", Text: "lonely", Indent: 2}}); len(run) != 1 {
		t.Errorf("single candidate run len = %d, want 1 (caller applies the >=2 threshold)", len(run))
	}
	if run := LongestRun(nil); run != nil {
		t.Errorf("nil input run = %+v, want nil", run)
	}
}
