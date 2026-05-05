package httpapi

import "testing"

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello world", 5, "hello"},
		{"hello", 10, "hello"},
		{"", 5, ""},
		{"你好世界，这是一个测试消息", 5, "你好世界，"},
		{"abc", 0, ""},
		{"🎉🎊🎈🎁", 2, "🎉🎊"},
		// Exactly n runes
		{"abcde", 5, "abcde"},
	}
	for _, tt := range tests {
		got := truncateRunes(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}
