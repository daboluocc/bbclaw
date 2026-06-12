package cmd

import "testing"

// IsNewerVersion is load-bearing for the admin page's "update_available" hint
// and `bbclaw-adapter update`'s "skip if already at" short-circuit. The cases
// here mirror the patterns we ship in real tags (CI build tags include the
// short SHA suffix, dev builds report the literal "dev").
func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.4.0", "v0.5.0", true},
		{"v0.5.0", "v0.5.0", false},
		{"v0.5.1", "v0.5.0", false},
		{"v0.4.18", "v0.5.0", true},
		{"v0.5.0-3-g1234abc", "v0.5.0", false}, // dev build at the tag → equal
		{"v0.5.0-3-g1234abc", "v0.5.1", true},
		{"dev", "v0.5.0", true},     // dev parses to 0.0.0 → always behind
		{"", "v0.5.0", true},        // empty parses to 0.0.0
		{"v0.5.0", "", false},       // unparseable latest stays silent
		{"0.5.0", "v0.5.1", true},   // leading v optional on either side
	}
	for _, c := range cases {
		got := IsNewerVersion(c.current, c.latest)
		if got != c.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}
