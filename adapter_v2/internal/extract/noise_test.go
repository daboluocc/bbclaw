package extract

import "testing"

// TestIsNoiseLine table-drives the per-line classifier across the three noise
// classes plus real content, including the post-emulator shapes (box borders
// already collapsed to blanks, "│" left-border kept or dropped).
func TestIsNoiseLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"blank", "", true},
		{"spaces only", "      ", true},

		{"empty idle prompt", " > ", true},
		{"empty idle prompt bare", ">", true},
		{"prompt with user text", " > what is the capital of France?", true},
		{"prompt with left border kept", "│ > hello there", true},
		{"prompt deeply indented", "        >  draft a poem", true},

		{"spinner full", "✶ Cogitating… (3s · ↑ 240 tokens · esc to interrupt)", true},
		{"spinner minimal", "  ✻ Working… (12s · esc to interrupt)", true},
		{"spinner glyph dropped by emulator", "Cogitating… (3s · esc to interrupt)", true},
		{"api retry wait", "Waiting for API response · will retry in 2m 26s · check your network", true},
		{"api retry short", "· will retry in 30s ·", true},
		{"token counter tail", "38 tokens)", true},
		{"token counter with arrow", "↑ 1.2k tokens", true},
		{"token counter with sep", "38 tokens · esc to interrupt", true},
		{"reply mentioning tokens (not chrome)", "你的余额还有 100 tokens 可用", false},

		{"box top border", "╭──────────────╮", true},
		{"box bottom border", "╰──────────────╯", true},
		{"horizontal rule", "────────────", true},
		{"block-element rule", "▔▔▔▔▔▔", true},
		{"box border with padding", "   ╭────╮   ", true},

		{"reply line", "The capital of France is Paris.", false},
		{"reply indented", "  and is its largest city.", false},
		{"reply bullet residue (leading space)", " The capital of France is Paris.", false},
		{"reply mentioning esc but not interrupt hint", "Press q to quit the pager.", false},
		{"reply with a greater-than mid-line", "x > y means x exceeds y", false},
		{"reply with box char inside prose", "draw a ╭ shape here", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoiseLine(tc.line); got != tc.want {
				t.Errorf("isNoiseLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestContentLinesTrimsEdges checks that noise removal drops blank rows (which
// in a VT grid are layout, not content — the reply may sit far from the prompt
// with a wall of blanks between) and box-border noise, leaving only the textual
// reply lines in order.
func TestContentLinesTrimsEdges(t *testing.T) {
	visible := "\n\n" + // leading blanks
		"Hello.\n" +
		"\n" + // interior blank -> dropped (grid layout, not content)
		"World.\n" +
		"╰────╯\n" + // box noise -> dropped
		"\n\n" // trailing blanks
	got := contentLines(visible)
	want := []string{"Hello.", "World."}
	if len(got) != len(want) {
		t.Fatalf("contentLines len = %d (%q), want %d (%q)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("contentLines[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
