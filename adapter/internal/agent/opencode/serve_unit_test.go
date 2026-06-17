package opencode

import "testing"

func TestCheckVersion(t *testing.T) {
	cases := []struct {
		ver string
		ok  bool
	}{
		{"1.15.0", true},
		{"1.15.1", true},
		{"1.20.3", true},
		{"1.29.99", true},
		{"1.14.9", false}, // below min minor
		{"1.30.0", false}, // at exclusive max
		{"2.0.0", false},  // wrong major
		{"0.19.2", false}, // wrong major
		{"unknown", true}, // unparseable → allowed (reachability already proven)
		{"v1.16.0", true}, // leading v tolerated
	}
	for _, c := range cases {
		err := checkVersion(c.ver)
		if (err == nil) != c.ok {
			t.Errorf("checkVersion(%q): ok=%v err=%v", c.ver, c.ok, err)
		}
	}
}

func TestSplitModel(t *testing.T) {
	cases := []struct {
		in        string
		prov, mod string
		ok        bool
	}{
		{"deepseek/deepseek-v4-pro", "deepseek", "deepseek-v4-pro", true},
		{"anthropic/claude-opus-4-8", "anthropic", "claude-opus-4-8", true},
		{"", "", "", false},
		{"justmodel", "", "", false},
		{"/x", "", "", false},
		{"p/", "", "", false},
	}
	for _, c := range cases {
		p, m, ok := splitModel(c.in)
		if ok != c.ok || p != c.prov || m != c.mod {
			t.Errorf("splitModel(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, p, m, ok, c.prov, c.mod, c.ok)
		}
	}
}

func TestPageBounds(t *testing.T) {
	cases := []struct {
		n, before, limit int
		lo, hi           int
	}{
		{10, 0, 5, 5, 10},  // latest page
		{10, 0, 50, 0, 10}, // limit > n
		{10, 5, 3, 2, 5},   // before cursor, capped
		{10, 5, 50, 0, 5},  // before cursor, limit > window
		{0, 0, 5, 0, 0},    // empty
		{3, 100, 5, 0, 3},  // before beyond n
	}
	for _, c := range cases {
		lo, hi := pageBounds(c.n, c.before, c.limit)
		if lo != c.lo || hi != c.hi {
			t.Errorf("pageBounds(%d,%d,%d) = (%d,%d), want (%d,%d)", c.n, c.before, c.limit, lo, hi, c.lo, c.hi)
		}
	}
}

func TestMapParts(t *testing.T) {
	parts := []ocMsgPart{
		{Type: "step-start"},
		{Type: "reasoning", Text: "thinking..."},
		{Type: "tool", Tool: "Bash"},
		{Type: "text", Text: "hello"},
		{Type: "step-finish"},
		{Type: "subtask", Tool: "worker"},
	}
	got := mapParts(parts)
	want := []string{"thinking", "tool", "text", "dispatch"}
	if len(got) != len(want) {
		t.Fatalf("mapParts len=%d want=%d (%+v)", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("part[%d].Kind=%q want %q", i, got[i].Kind, k)
		}
	}
}
