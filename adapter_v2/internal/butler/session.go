package butler

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	isClaude := len(out) > 0 && strings.Contains(strings.ToLower(filepath.Base(out[0])), "claude")
	if isClaude {
		switch {
		case d.activeID == "":
			out = append(out, "--continue") // no known id yet → resume the latest
		case d.fresh:
			out = append(out, "--session-id", d.activeID) // brand-new conversation
		default:
			out = append(out, "--resume", d.activeID) // resume a specific conversation
		}
	}
	cfg := ptyhost.Config{Argv: out, Cwd: d.cwd}
	if isClaude {
		cfg.StartupInput = claudeStartupKeys()
	}
	return cfg
}

// claudeStartupKeys returns the keystrokes injected after a claude PTY spawns to
// auto-dismiss its first-run "Try the new fullscreen renderer?" upsell (driven by
// fullscreenUpsellSeenCount in ~/.claude.json) and any similar blocking prompt:
// a few Enters, each picking the highlighted default, staggered so they land
// after the TUI has painted (claude can take a second or two, longer when it
// --resumes a conversation). The voice/device path has no human to answer it, so
// without this the shared session hangs on the upsell. Extra Enters that land on
// the empty main prompt are harmless no-ops. Disable with
// ADAPTER_V2_CLAUDE_AUTO_ENTER=0; returns nil when disabled.
func claudeStartupKeys() []ptyhost.StartupChunk {
	if !envBool("ADAPTER_V2_CLAUDE_AUTO_ENTER", true) {
		return nil
	}
	// First Enter waits out claude's startup paint; the rest follow on a steady
	// cadence to cover a slow/late-painting prompt (~1.8s, 3.3s, 4.8s, 6.3s).
	const gap = 1500 * time.Millisecond
	delays := []time.Duration{1800 * time.Millisecond, gap, gap, gap}
	keys := make([]ptyhost.StartupChunk, 0, len(delays))
	for _, d := range delays {
		keys = append(keys, ptyhost.StartupChunk{Delay: d, Data: []byte("\r")})
	}
	return keys
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

// ConversationMessage is one rendered turn for the device/web history view
// (agent.messages). content is plain text — rows that carry only a tool_use /
// tool_result / thinking block (no speakable text) are dropped.
type ConversationMessage struct {
	Role      string // "user" | "assistant"
	Content   string // plain text
	Timestamp string // RFC3339 if the .jsonl row has one
	Seq       int    // chronological index (0-based) over the kept messages
}

// messageContentMax caps a single message's text (rune-safe) for the small device
// screen / PSRAM.
const messageContentMax = 4096

// Messages parses a conversation's claude .jsonl into rendered messages, in
// chronological order. A missing file (or unknown encoding) yields an empty slice,
// not an error — the device shows a blank history rather than failing.
func (d *DeviceSession) Messages(id string) ([]ConversationMessage, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	dir := claudeProjectDir(d.cwd)
	if dir == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		return nil, nil // missing conversation => empty page
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // assistant turns can be large
	var out []ConversationMessage
	for sc.Scan() {
		var rec struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		role := rec.Message.Role
		if role == "" {
			role = rec.Type
		}
		if role != "user" && role != "assistant" {
			continue
		}
		// firstText returns "" for tool_result / tool_use / thinking-only content,
		// so reusing it drops exactly the non-speakable rows.
		text := firstText(rec.Message.Content)
		if text == "" {
			continue
		}
		out = append(out, ConversationMessage{
			Role:      role,
			Content:   capRunes(text, messageContentMax),
			Timestamp: rec.Timestamp,
			Seq:       len(out),
		})
	}
	return out, nil
}

// PageMessages returns a backward page of msgs (0-indexed chronological): before<=0
// is the newest page (ending at total); else end=min(before,total), start=
// max(0,end-limit). hasMore reports older messages remain. limit is clamped to
// [1,200] (default 50).
func PageMessages(all []ConversationMessage, before, limit int) (page []ConversationMessage, total int, hasMore bool) {
	total = len(all)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	end := total
	if before > 0 && before < total {
		end = before
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return all[start:end], total, start > 0
}

// capRunes hard-caps s to n runes (rune-safe).
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
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
		// Skip the v1 claude-code pool's warm-up probe conversations. v1 and v2
		// SHARE this workspace (#231), so the pool's noop ("respond with the single
		// word: ready", adapter/internal/agent/claudecode/pool.go) litters the dir
		// with thousands of throwaway .jsonl — they are NOT real sessions, and the
		// most-recent one would otherwise be picked as the active conversation,
		// shadowing the user's real butler memory.
		first := firstUserText(filepath.Join(dir, name))
		if strings.HasPrefix(strings.TrimSpace(first), v1PoolNoopProbe) {
			continue
		}
		info, err := e.Info()
		var mod int64
		if err == nil {
			mod = info.ModTime().Unix()
		}
		title := truncateTitle(first)
		if title == "" {
			title = shortID(id)
		}
		out = append(out, ConversationInfo{ID: id, Title: title, ModUnixSec: mod})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModUnixSec > out[j].ModUnixSec })
	return out, nil
}

// v1PoolNoopProbe is v1's claude-code pool warm-up prompt; conversations that start
// with it are throwaway health probes, not real sessions (see listConversations).
const v1PoolNoopProbe = "respond with the single word: ready"

// firstUserText returns the first user message of a conversation .jsonl (full, not
// truncated), or "" if none. claude writes one JSON object per line; a user turn is
// {"type":"user","message":{"role":"user","content":...}} where content is a string
// or an array of {type:"text",text:...} blocks.
func firstUserText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
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
			return t
		}
	}
	return ""
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
