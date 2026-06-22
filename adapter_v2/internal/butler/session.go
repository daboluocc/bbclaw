package butler

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
	"github.com/google/uuid"
)

// DeviceSession owns the lifecycle of the shared default conversation (ADR-032).
// One conversation = one claude session (a <uuid>.jsonl in the workspace). It is
// the single source of truth for the ACTIVE conversation id, so every entry point
// that (re)creates session.DefaultID spawns claude with the SAME resume flag, and
// the device can list / start / switch conversations coherently.
//
// Switching = respawn: New/Resume update the active id (persisted) and kill the
// live default session via the Manager, so the next spawn uses the new resume
// flag. The cloud-relay bridge auto-rebuilds (its dead-session eviction) and a web
// terminal reconnects.
type DeviceSession struct {
	mgr      *session.Manager
	baseArgv []string // persona + permissions, WITHOUT the resume flag
	cwd      string   // the butler workspace

	mu       sync.Mutex
	activeID string // claude session uuid; "" => --continue (resume latest)
	fresh    bool   // true: activeID is a brand-new conversation (--session-id), not --resume
}

// NewDeviceSession builds the holder, loading the persisted active id (or, absent
// that, the most recent conversation in the workspace) so a restart resumes the
// same conversation the user was last in.
func NewDeviceSession(mgr *session.Manager, baseArgv []string, cwd string) *DeviceSession {
	d := &DeviceSession{mgr: mgr, baseArgv: baseArgv, cwd: cwd}
	sessions, _ := listConversations(cwd)
	exists := func(id string) bool {
		for _, s := range sessions {
			if s.ID == id {
				return true
			}
		}
		return false
	}
	// Prefer the persisted active conversation, but only if it still exists on disk
	// (a deleted/invalid id would make claude --resume fail/hang). Else the most
	// recent conversation. Else — a fresh workspace with no history — mint a NEW
	// one. We never use a bare --continue: it hangs when there's nothing to resume
	// (verified against real claude), and an explicit id is what the active-id
	// reporting / isolation needs.
	id := loadActiveSession()
	switch {
	case id != "" && exists(id):
		d.activeID, d.fresh = id, false // resume persisted
	case len(sessions) > 0:
		d.activeID, d.fresh = sessions[0].ID, false // resume most recent
	default:
		d.activeID, d.fresh = uuid.New().String(), true // fresh workspace → new conversation
	}
	return d
}

// Config is the ptyhost.Config for spawning session.DefaultID. All entry points
// use this so the default PTY always runs the active conversation.
func (d *DeviceSession) Config() ptyhost.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string{}, d.baseArgv...)
	// The resume flags (--continue / --resume / --session-id) are claude-specific;
	// don't append them to another CLI (e.g. a test's `cat`, which would treat
	// "--continue" as a filename and exit).
	if len(out) > 0 && strings.Contains(strings.ToLower(filepath.Base(out[0])), "claude") {
		switch {
		case d.activeID == "":
			out = append(out, "--continue") // no known id yet → resume the latest
		case d.fresh:
			out = append(out, "--session-id", d.activeID) // brand-new conversation
		default:
			out = append(out, "--resume", d.activeID) // resume a specific conversation
		}
	}
	return ptyhost.Config{Argv: out, Cwd: d.cwd}
}

// ActiveID returns the active conversation id (may be "" before the first spawn
// when resuming the latest via --continue).
func (d *DeviceSession) ActiveID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeID
}

// New starts a fresh conversation: mint a new id, persist it, and respawn the
// default session with --session-id. Returns the new id.
func (d *DeviceSession) New() string {
	id := uuid.New().String()
	d.mu.Lock()
	d.activeID = id
	d.fresh = true
	d.mu.Unlock()
	saveActiveSession(id)
	d.respawn()
	return id
}

// Resume switches to an existing conversation: set it active, persist, and respawn
// with --resume <id>.
func (d *DeviceSession) Resume(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	d.mu.Lock()
	d.activeID = id
	d.fresh = false
	d.mu.Unlock()
	saveActiveSession(id)
	d.respawn()
}

// respawn kills the live default session so the next spawn picks up the new
// resume flag. Lazy: the cloud relay rebuilds on the next turn; a web terminal
// reconnects.
func (d *DeviceSession) respawn() {
	d.mgr.Remove(session.DefaultID)
}

// List returns the workspace's conversations, most-recent first.
func (d *DeviceSession) List() ([]ConversationInfo, error) {
	return listConversations(d.cwd)
}

// ConversationInfo is one claude conversation for the device's history picker.
type ConversationInfo struct {
	ID         string // claude session uuid
	Title      string // first user message (truncated), or the id
	ModUnixSec int64  // .jsonl mtime, for ordering / lastUsed display
}

// ── claude conversation storage ─────────────────────────────────────────────

// claudeProjectDir maps a cwd to claude's per-project conversation directory
// (~/.claude/projects/<cwd with '/' and '.' replaced by '-'>). This encoding is
// claude-internal; if it ever changes, listing degrades to empty (callers treat
// that as "no history", which is safe).
func claudeProjectDir(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	enc := strings.ReplaceAll(cwd, "/", "-")
	enc = strings.ReplaceAll(enc, ".", "-")
	return filepath.Join(home, ".claude", "projects", enc)
}

func listConversations(cwd string) ([]ConversationInfo, error) {
	dir := claudeProjectDir(cwd)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // no project dir yet => no history
	}
	var out []ConversationInfo
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		info, err := e.Info()
		var mod int64
		if err == nil {
			mod = info.ModTime().Unix()
		}
		out = append(out, ConversationInfo{
			ID:         id,
			Title:      conversationTitle(filepath.Join(dir, name), id),
			ModUnixSec: mod,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModUnixSec > out[j].ModUnixSec })
	return out, nil
}

// conversationTitle reads the first user message from a conversation .jsonl as a
// human label, falling back to the id. claude writes one JSON object per line;
// a user turn is {"type":"user","message":{"role":"user","content":...}} where
// content is a string or an array of {type:"text",text:...} blocks.
func conversationTitle(path, id string) string {
	f, err := os.Open(path)
	if err != nil {
		return shortID(id)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type != "user" && rec.Message.Role != "user" {
			continue
		}
		if t := firstText(rec.Message.Content); t != "" {
			return truncateTitle(t)
		}
	}
	return shortID(id)
}

// firstText extracts text from a claude content field (string, or []{type,text}).
func firstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

func truncateTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace/newlines
	r := []rune(s)
	if len(r) > 24 {
		return string(r[:24]) + "…"
	}
	return s
}

func shortID(id string) string {
	if len(id) >= 8 {
		return "对话-" + id[:8]
	}
	return "对话-" + id
}

// ── active-session persistence (~/.bbclaw-adapter-v2/active-session.json) ─────

type activeSessionFile struct {
	ActiveSessionID string `json:"activeSessionId"`
}

func activeSessionPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bbclaw-adapter-v2", "active-session.json")
}

func loadActiveSession() string {
	if id := strings.TrimSpace(os.Getenv("ADAPTER_V2_ACTIVE_SESSION")); id != "" {
		return id
	}
	p := activeSessionPath()
	if p == "" {
		return ""
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var f activeSessionFile
	if json.Unmarshal(raw, &f) != nil {
		return ""
	}
	return strings.TrimSpace(f.ActiveSessionID)
}

func saveActiveSession(id string) {
	p := activeSessionPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	data, _ := json.Marshal(activeSessionFile{ActiveSessionID: id})
	_ = os.WriteFile(p, data, 0o600)
}
