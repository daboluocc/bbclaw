package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func TestListSessions(t *testing.T) {
	// Create temp directory for test sessions
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create test session files
	sessions := []struct {
		pid       int
		sessionID string
		cwd       string
		startedAt int64
		updatedAt int64
	}{
		{1001, "session-1", "/Users/test/project1", 1714000000000, 1714000100000},
		{1002, "session-2", "/Users/test/project2", 1714000200000, 1714000300000},
		{1003, "session-3", "/Users/test/project3", 1714000400000, 0}, // no updatedAt
	}

	for _, s := range sessions {
		data := map[string]any{
			"pid":       s.pid,
			"sessionId": s.sessionID,
			"cwd":       s.cwd,
			"startedAt": s.startedAt,
		}
		if s.updatedAt > 0 {
			data["updatedAt"] = s.updatedAt
		}
		b, _ := json.Marshal(data)
		path := filepath.Join(sessionsDir, s.sessionID+".json")
		if err := os.WriteFile(path, b, 0644); err != nil {
			t.Fatal(err)
		}

		// Create conversation history with a user message
		projectDir := cwdToProjectDir(s.cwd)
		historyDir := filepath.Join(projectsDir, projectDir)
		if err := os.MkdirAll(historyDir, 0755); err != nil {
			t.Fatal(err)
		}
		historyPath := filepath.Join(historyDir, s.sessionID+".jsonl")
		userMsg := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": "This is a test message for " + s.sessionID,
			},
		}
		msgBytes, _ := json.Marshal(userMsg)
		if err := os.WriteFile(historyPath, msgBytes, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Set env var to point to test directory
	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)
	t.Setenv("HOME", tmpDir)

	// Create driver
	d := New(Options{}, obs.NewLogger())

	// Test listing sessions
	ctx := context.Background()
	result, err := d.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	// Should return 3 sessions
	if len(result) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(result))
	}

	// Should be sorted by LastUsed descending (most recent first)
	// session-3: 1714000400 (startedAt only)
	// session-2: 1714000300 (updatedAt)
	// session-1: 1714000100 (updatedAt)
	if result[0].ID != "session-3" {
		t.Errorf("expected first session to be session-3, got %s", result[0].ID)
	}
	if result[1].ID != "session-2" {
		t.Errorf("expected second session to be session-2, got %s", result[1].ID)
	}
	if result[2].ID != "session-1" {
		t.Errorf("expected third session to be session-1, got %s", result[2].ID)
	}

	// Check LastUsed values (in seconds)
	if result[0].LastUsed != 1714000400 {
		t.Errorf("expected session-3 lastUsed=1714000400, got %d", result[0].LastUsed)
	}
	if result[1].LastUsed != 1714000300 {
		t.Errorf("expected session-2 lastUsed=1714000300, got %d", result[1].LastUsed)
	}

	// Check preview extraction
	if result[0].Preview != "This is a test messa…" {
		t.Errorf("expected preview to be truncated to 20 chars, got %q", result[0].Preview)
	}

	// Test limit parameter
	limited, err := d.ListSessions(ctx, 2)
	if err != nil {
		t.Fatalf("ListSessions with limit failed: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected 2 sessions with limit=2, got %d", len(limited))
	}
}

func TestListSessions_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)

	d := New(Options{}, obs.NewLogger())
	result, err := d.ListSessions(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(result))
	}
}

func TestListSessions_DirNotExist(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "nonexistent")

	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)

	d := New(Options{}, obs.NewLogger())
	result, err := d.ListSessions(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListSessions should not fail when dir doesn't exist: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 sessions when dir doesn't exist, got %d", len(result))
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		input    string
		maxRunes int
		expected string
	}{
		{"hello world", 20, "hello world"},
		{"hello world", 5, "hello…"},
		{"你好世界", 2, "你好…"},
		{"", 10, ""},
		{"a", 1, "a"},
		{"ab", 1, "a…"},
	}

	for _, tt := range tests {
		result := truncateUTF8(tt.input, tt.maxRunes)
		if result != tt.expected {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tt.input, tt.maxRunes, result, tt.expected)
		}
	}
}

func TestCwdToProjectDir(t *testing.T) {
	tests := []struct {
		cwd      string
		expected string
	}{
		{"/Users/foo/bar", "-Users-foo-bar"},
		{"/home/user/project", "-home-user-project"},
		{"C:\\Users\\foo\\bar", "C:-Users-foo-bar"}, // Windows path
	}

	for _, tt := range tests {
		result := cwdToProjectDir(tt.cwd)
		if result != tt.expected {
			t.Errorf("cwdToProjectDir(%q) = %q, want %q", tt.cwd, result, tt.expected)
		}
	}
}
