package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// maxContentBytes caps a single message's content length. The device has a
// small screen and limited memory; very long markdown is useless there.
const maxContentBytes = 4096

// LoadMessages implements agent.MessageLoader by reading the session's JSONL
// conversation transcript at ~/.claude/projects/{project}/{sid}.jsonl.
//
// The adapter keeps the on-disk file as the source of truth and re-scans it
// per call — sessions are small enough (kB to low MB) that this is fine for
// the device's pagination cadence (one page per scroll-to-top).
func (d *Driver) LoadMessages(ctx context.Context, sid string, before, limit int) (agent.MessagesPage, error) {
	if strings.TrimSpace(sid) == "" {
		return agent.MessagesPage{}, fmt.Errorf("claude-code: empty session id")
	}
	if limit <= 0 {
		limit = 50
	}

	historyPath, err := d.findHistoryPath(sid)
	if err != nil {
		return agent.MessagesPage{}, err
	}
	if historyPath == "" {
		return agent.MessagesPage{Messages: []agent.Message{}}, nil
	}

	all, err := readAllMessages(historyPath)
	if err != nil {
		return agent.MessagesPage{}, fmt.Errorf("claude-code: read transcript %s: %w", historyPath, err)
	}

	total := len(all)
	if total == 0 {
		return agent.MessagesPage{Messages: []agent.Message{}}, nil
	}

	// Resolve slice [start, end) in original chronological order.
	var end int
	if before <= 0 {
		end = total
	} else if before > total {
		end = total
	} else {
		end = before
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	if start >= end {
		return agent.MessagesPage{Messages: []agent.Message{}, Total: total, HasMore: false}, nil
	}

	page := agent.MessagesPage{
		Messages: all[start:end],
		Total:    total,
		HasMore:  start > 0,
	}
	return page, nil
}

// findHistoryPath returns the absolute path to the JSONL transcript for sid,
// or "" if it cannot be located.
//
// We can't trust ~/.claude/sessions/*.json for the lookup because those files
// are keyed by PID (one per running claude-code process), not by sessionId, and
// they're rotated/removed when the process exits. A historical session whose
// process is long gone has no entry there, so we'd never find its cwd.
//
// Instead, scan projects/*/{sid}.jsonl directly — the on-disk transcripts
// are the authoritative record, and the file basename IS the sessionId.
// O(N) in number of project subdirs (typically tens), each iteration is one
// stat() — fast enough for the per-page-load cadence.
func (d *Driver) findHistoryPath(sid string) (string, error) {
	sessionsDir := os.Getenv("CLAUDE_SESSIONS_DIR")
	var projectsDir string
	if sessionsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("claude-code: home dir: %w", err)
		}
		projectsDir = filepath.Join(home, ".claude", "projects")
	} else {
		projectsDir = filepath.Join(filepath.Dir(sessionsDir), "projects")
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("claude-code: read projects dir %s: %w", projectsDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, entry.Name(), sid+".jsonl")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	return "", nil
}

// readAllMessages parses a Claude JSONL transcript end-to-end and returns
// every user/assistant message in chronological order. Lines that aren't
// recognized message records (e.g. tool_call, system) are skipped silently.
// Malformed lines are skipped rather than failing the whole load.
func readAllMessages(path string) ([]agent.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []agent.Message
	scanner := bufio.NewScanner(f)
	// Some assistant messages contain very long content; raise the line cap.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		msg, ok := decodeMessage(line)
		if !ok {
			continue
		}
		msg.Seq = len(out)
		out = append(out, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeMessage extracts a {role, content} pair from one JSONL row. Returns
// ok=false for rows that aren't user/assistant text turns.
func decodeMessage(line string) (agent.Message, bool) {
	var raw struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return agent.Message{}, false
	}

	role := strings.ToLower(strings.TrimSpace(raw.Type))
	switch role {
	case "user", "assistant":
		// pass through
	default:
		return agent.Message{}, false
	}

	content, ok := decodeContent(raw.Message)
	if !ok {
		return agent.Message{}, false
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return agent.Message{}, false
	}
	if len(content) > maxContentBytes {
		content = safeTruncateBytes(content, maxContentBytes) + "…"
	}
	return agent.Message{Role: role, Content: content}, true
}

// decodeContent flattens the various shapes Claude uses for `message.content`
// into a single text string. Returns ok=false when no usable text was found
// (e.g. the message was a pure tool_use block).
func decodeContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}

	// Shape 1: {"role":"user","content":"plain string"}
	var withString struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &withString); err == nil && withString.Content != "" {
		return withString.Content, true
	}

	// Shape 2: {"role":"assistant","content":[{"type":"text","text":"..."},{"type":"tool_use",...}]}
	var withArray struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &withArray); err == nil {
		var parts []string
		for _, b := range withArray.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), true
		}
	}

	return "", false
}

// safeTruncateBytes truncates s to <= maxBytes without splitting a UTF-8
// rune. Used to enforce the per-message byte cap before serializing to
// the wire.
func safeTruncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk back to a rune boundary.
	for i := maxBytes; i > 0; i-- {
		if (s[i]&0xC0) != 0x80 && i <= len(s) {
			return s[:i]
		}
	}
	return s[:maxBytes]
}
