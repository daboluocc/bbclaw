package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// ListSessions implements agent.SessionLister by reading session metadata
// from the claude CLI's session directory (default ~/.claude/sessions/).
func (d *Driver) ListSessions(ctx context.Context, limit int) ([]agent.SessionInfo, error) {
	sessionsDir := os.Getenv("CLAUDE_SESSIONS_DIR")
	if sessionsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("claude-code: get home dir: %w", err)
		}
		sessionsDir = filepath.Join(home, ".claude", "sessions")
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []agent.SessionInfo{}, nil
		}
		return nil, fmt.Errorf("claude-code: read sessions dir: %w", err)
	}

	var sessions []sessionMetadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(sessionsDir, entry.Name())
		meta, err := parseSessionFile(path)
		if err != nil {
			d.log.Warnf("claude-code: skip unparseable session %s: %v", entry.Name(), err)
			continue
		}
		sessions = append(sessions, meta)
	}

	// Sort by LastUsed descending (most recent first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastUsed > sessions[j].LastUsed
	})

	// Apply limit
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}

	// Convert to agent.SessionInfo and fetch previews
	result := make([]agent.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		preview := d.getSessionPreview(s.SessionID, s.Cwd)
		result = append(result, agent.SessionInfo{
			ID:           s.SessionID,
			Preview:      preview,
			LastUsed:     s.LastUsed,
			MessageCount: s.MessageCount,
			Cwd:          filepath.Base(s.Cwd),
		})
	}

	return result, nil
}

// sessionMetadata mirrors the subset of fields we care about from the
// ~/.claude/sessions/*.json files.
type sessionMetadata struct {
	SessionID    string `json:"sessionId"`
	Cwd          string `json:"cwd"`
	StartedAt    int64  `json:"startedAt"`
	UpdatedAt    int64  `json:"updatedAt,omitempty"`
	LastUsed     int64  // derived: max(UpdatedAt, StartedAt)
	MessageCount int    // computed from JSONL
}

func parseSessionFile(path string) (sessionMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionMetadata{}, err
	}
	var meta sessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return sessionMetadata{}, err
	}
	// Derive LastUsed: prefer UpdatedAt if present, else StartedAt
	meta.LastUsed = meta.StartedAt / 1000 // convert ms to seconds
	if meta.UpdatedAt > 0 {
		meta.LastUsed = meta.UpdatedAt / 1000
	}
	return meta, nil
}

// getSessionPreview attempts to extract the first user message from the
// session's JSONL conversation history. Returns empty string if not found.
func (d *Driver) getSessionPreview(sessionID, cwd string) string {
	// Claude stores conversation history in ~/.claude/projects/{project-dir}/{sessionId}.jsonl
	// The project-dir is derived from cwd (e.g. /Users/foo/bar -> -Users-foo-bar)

	// Determine the base directory (same parent as sessions directory)
	sessionsDir := os.Getenv("CLAUDE_SESSIONS_DIR")
	var projectsDir string
	if sessionsDir != "" {
		// If CLAUDE_SESSIONS_DIR is set, derive projects dir from same parent
		// e.g. /custom/sessions -> /custom/projects
		parent := filepath.Dir(sessionsDir)
		projectsDir = filepath.Join(parent, "projects")
	} else {
		// Default: ~/.claude/projects
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		projectsDir = filepath.Join(home, ".claude", "projects")
	}

	projectDir := cwdToProjectDir(cwd)
	historyPath := filepath.Join(projectsDir, projectDir, sessionID+".jsonl")

	f, err := os.Open(historyPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Read first few KB to find the first user message
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	lines := strings.Split(string(buf[:n]), "\n")

	messageCount := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Type == "user" && entry.Message.Role == "user" {
			messageCount++
			if messageCount == 1 {
				// First user message: truncate to 20 chars (UTF-8 safe)
				preview := strings.TrimSpace(entry.Message.Content)
				return truncateUTF8(preview, 20)
			}
		}
	}
	return ""
}

// cwdToProjectDir converts a working directory path to the project directory
// name used by claude CLI (e.g. /Users/foo/bar -> -Users-foo-bar).
func cwdToProjectDir(cwd string) string {
	// Replace both Unix and Windows path separators with hyphens
	result := strings.ReplaceAll(cwd, "/", "-")
	result = strings.ReplaceAll(result, "\\", "-")
	return result
}

// truncateUTF8 safely truncates a string to maxRunes runes, avoiding
// splitting multi-byte UTF-8 sequences. Appends "…" if truncated.
func truncateUTF8(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
