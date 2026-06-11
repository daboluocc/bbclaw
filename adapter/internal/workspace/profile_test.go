package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseProfileStatus(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"uninitialized", "# 档案\n\n<!-- STATUS: uninitialized -->\n", ProfileStatusUninitialized},
		{"initialized", "<!-- STATUS: initialized -->", ProfileStatusInitialized},
		{"skipped", "<!-- STATUS: skipped -->", ProfileStatusSkipped},
		{"extra whitespace", "<!-- STATUS:   initialized   -->", ProfileStatusInitialized},
		{"marker absent", "# 档案\n- 怎么称呼:\n", ""},
		{"unterminated marker", "<!-- STATUS: initialized", ""},
		{"empty content", "", ""},
		{"skeleton full", "# 用户身份档案\n\n<!-- 由 BBClaw 管家在初次见面时通过对话填写。修改 STATUS 标记其状态： -->\n<!-- uninitialized=尚未初始化 / initialized=已录入 / skipped=用户选择跳过。 -->\n<!-- STATUS: uninitialized -->\n\n- 怎么称呼：\n", ProfileStatusUninitialized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseProfileStatus(tc.content); got != tc.want {
				t.Fatalf("ParseProfileStatus(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func TestProfileStatusReadsWorkspaceFile(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BBCLAW_DATA_DIR", dataDir)

	// Missing file → unknown.
	if got := ProfileStatus(); got != "" {
		t.Fatalf("missing profile.md: got %q, want \"\"", got)
	}

	memDir := filepath.Join(dataDir, "workspace", "MEMORY")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(memDir, "profile.md")
	if err := os.WriteFile(path, []byte("<!-- STATUS: uninitialized -->\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProfileStatus(); got != ProfileStatusUninitialized {
		t.Fatalf("got %q, want %q", got, ProfileStatusUninitialized)
	}

	if err := os.WriteFile(path, []byte("<!-- STATUS: initialized -->\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProfileStatus(); got != ProfileStatusInitialized {
		t.Fatalf("got %q, want %q", got, ProfileStatusInitialized)
	}
}
