// Package claudecode implements agent.Driver by spawning the `claude` CLI
// in `-p --output-format stream-json --verbose` mode and translating its
// NDJSON output into the unified agent.Event stream.
//
// Phase 1 scope (see design/agent_bus.md):
//   - one-shot per Send: spawn a fresh subprocess, carry session continuity
//     via --resume using the session_id Claude emits in its init event
//   - emit EvText for assistant text blocks, EvTurnEnd on result, EvError
//     on failures. tool_use frames are surfaced as EvToolCall *display-only*
//     events — Capabilities().ToolApproval stays false because the
//     approval round-trip (Phase 2) is not yet wired.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

const (
	driverName   = "claude-code"
	defaultBin   = "claude"
	eventBufSize = 64
)

// Driver is the claude-code AgentDriver implementation.
type Driver struct {
	bin    string
	log    *obs.Logger
	extra  []string

	mu       sync.Mutex
	sessions map[agent.SessionID]*session
}

// Options configures the driver.
type Options struct {
	// Bin is the path to the `claude` binary; empty defaults to "claude"
	// resolved on PATH.
	Bin string
	// ExtraArgs appended after the fixed args (e.g. "--model",
	// "claude-sonnet-4-6"). Do not include `-p` or `--output-format` —
	// the driver sets those itself.
	ExtraArgs []string
}

// New constructs a Driver. The logger is required; pass obs.NewLogger() if
// you don't have one.
func New(opts Options, log *obs.Logger) *Driver {
	bin := strings.TrimSpace(opts.Bin)
	if bin == "" {
		bin = defaultBin
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		log.Warnf("claude-code: binary %q not on PATH, will use as-is (%v)", bin, err)
	} else {
		bin = resolved
		log.Infof("claude-code: resolved binary %q", bin)
	}
	return &Driver{
		bin:      bin,
		log:      log,
		extra:    append([]string(nil), opts.ExtraArgs...),
		sessions: make(map[agent.SessionID]*session),
	}
}

// Name implements agent.Driver.
func (d *Driver) Name() string { return driverName }

// Capabilities implements agent.Driver. Phase 1 advertises what is actually
// wired: streaming and resume work, tool approval is not yet plumbed.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ToolApproval:  false,
		Resume:        true,
		Streaming:     true,
		MaxInputBytes: 64 * 1024,
	}
}

// Start allocates a new session. No subprocess is spawned here; the CLI is
// invoked on demand in Send so each turn can carry the latest --resume id.
//
// When opts.ResumeID is set, we use it AS the device-visible session id —
// not just as a `--resume` argument. This keeps the device's sessionId in
// lockstep with the on-disk JSONL filename: a session picked from the picker
// (sid=719a6a7e...) continues writing to that same JSONL after resume,
// instead of being silently re-numbered to `cc-<new-uuid>`. Without this,
// every resume looked like "isNew=1" to the firmware and the agent had no
// memory of prior turns.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	var sid agent.SessionID
	if strings.TrimSpace(opts.ResumeID) != "" {
		sid = agent.SessionID(strings.TrimSpace(opts.ResumeID))
	} else {
		sid = agent.SessionID("cc-" + uuid.NewString())
	}
	s := &session{
		id:        sid,
		events:    make(chan agent.Event, eventBufSize),
		resumeID:  opts.ResumeID,
		cwd:       opts.Cwd,
		env:       opts.Env,
		rootCtx:   ctx,
	}
	d.mu.Lock()
	d.sessions[sid] = s
	d.mu.Unlock()
	return sid, nil
}

// Events returns the session's event channel. Closed when the session ends.
func (d *Driver) Events(sid agent.SessionID) <-chan agent.Event {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		ch := make(chan agent.Event)
		close(ch)
		return ch
	}
	return s.events
}

// Send spawns `claude -p <text> ...` for this turn and streams its
// stream-json output onto the session's event channel. Blocks until the
// subprocess exits (caller should invoke Send in a goroutine if they want
// to keep reading Events concurrently; events are buffered anyway).
func (d *Driver) Send(sid agent.SessionID, text string) (sendErr error) {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

	// On early exit the handler is blocked on <-events waiting for
	// EvTurnEnd — emit it so the handler can tear down cleanly.
	defer func() {
		if sendErr != nil {
			s.emit(agent.Event{Type: agent.EvError, Text: sendErr.Error()})
			s.emit(agent.Event{Type: agent.EvTurnEnd})
		}
	}()

	args := []string{"-p", text, "--output-format", "stream-json", "--verbose"}
	if s.resumeID != "" {
		args = append(args, "--resume", s.resumeID)
	}
	args = append(args, d.extra...)

	ctx, cancel := context.WithCancel(s.rootCtx)
	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Dir = s.cwd
	if len(s.env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), s.env)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("claude-code: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("claude-code: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("claude-code: start %s: %w", d.bin, err)
	}

	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	d.log.Infof("claude-code: spawned sid=%s resume=%q pid=%d", sid, s.resumeID, cmd.Process.Pid)

	// Capture stderr while logging it: if claude-code refuses to resume a
	// locked session, we want to surface SESSION_BUSY to the device rather
	// than the bare "claude-code exit: 1" we'd otherwise emit.
	stderrCap := &stderrCapture{}
	stderrDone := make(chan struct{})
	go func() {
		drainStderr(stderr, d.log, sid, stderrCap)
		close(stderrDone)
	}()

	// Parse stdout stream-json, emitting events.
	parseStreamJSON(stdout, s, d.log)

	waitErr := cmd.Wait()
	<-stderrDone
	if waitErr != nil {
		snap := stderrCap.snapshot()
		switch {
		case snap.SessionBusy:
			s.emit(agent.Event{Type: agent.EvError, Text: "SESSION_BUSY: another process is using this session — close it or pick a different one"})
		case snap.SessionNotFound:
			s.emit(agent.Event{Type: agent.EvError, Text: "SESSION_NOT_FOUND: cli conversation no longer exists; adapter should mint a new session"})
		default:
			s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("claude-code exit: %v", waitErr)})
		}
	}
	s.emit(agent.Event{Type: agent.EvTurnEnd})
	return nil
}

// Approve is not yet supported; returns ErrUnsupported per Capabilities.
func (d *Driver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return agent.ErrUnsupported
}

// Stop terminates any in-flight subprocess and closes the session.
func (d *Driver) Stop(sid agent.SessionID) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	delete(d.sessions, sid)
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	close(s.events)
	return nil
}

// ─── session ────────────────────────────────────────────────────────────

type session struct {
	id       agent.SessionID
	events   chan agent.Event
	resumeID string
	cwd      string
	env      map[string]string
	rootCtx  context.Context

	seq uint64

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *session) emit(e agent.Event) {
	e.Seq = atomic.AddUint64(&s.seq, 1)
	select {
	case s.events <- e:
	case <-s.rootCtx.Done():
	}
}

func (s *session) setResumeID(id string) {
	s.mu.Lock()
	s.resumeID = id
	s.mu.Unlock()
}

// ─── stream-json parser ─────────────────────────────────────────────────

// claude-code stream-json schema (the subset we care about for Phase 1):
//
//   {"type":"system","subtype":"init","session_id":"...", ...}
//   {"type":"assistant","message":{"content":[{"type":"text","text":"..."},
//                                              {"type":"tool_use", ...}]}}
//   {"type":"user","message":{"content":[{"type":"tool_result", ...}]}}
//   {"type":"result","subtype":"success","result":"...","usage":{...}}
//
// We emit:
//   - EvText for each assistant text block
//   - EvTokens on result.usage
//   - EvError on any line we can't parse (logged, not fatal)
//   - tool_use / tool_result are logged only — EvToolCall plumbing is Phase 2

type streamEnvelope struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Message   *streamMessage  `json:"message,omitempty"`
	Result    string          `json:"result,omitempty"`
	Usage     *streamUsage    `json:"usage,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type streamMessage struct {
	Content []streamContent `json:"content"`
}

type streamContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	ID    string          `json:"id,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type streamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func parseStreamJSON(r io.Reader, s *session, log *obs.Logger) {
	sc := bufio.NewScanner(r)
	// stream-json can emit long lines (large tool_result payloads).
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env streamEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			log.Warnf("claude-code: unparseable line sid=%s err=%v line=%q", s.id, err, truncate(string(line), 200))
			continue
		}
		switch env.Type {
		case "system":
			if env.Subtype == "init" && env.SessionID != "" {
				s.setResumeID(env.SessionID)
				log.Infof("claude-code: session init sid=%s cli_session=%s", s.id, env.SessionID)
			}
		case "assistant":
			if env.Message == nil {
				continue
			}
			for _, c := range env.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						s.emit(agent.Event{Type: agent.EvText, Text: c.Text})
					}
				case "tool_use":
					// Display-only: we surface tool_use frames as EvToolCall
					// so the playground / device UI can render them, but
					// Capabilities().ToolApproval stays false because the
					// approval round-trip is not yet implemented (Phase 2).
					hint := summarizeToolInput(c.Name, c.Input)
					log.Infof("claude-code: tool_use sid=%s tool=%s id=%s hint=%q", s.id, c.Name, c.ID, hint)
					s.emit(agent.Event{
						Type: agent.EvToolCall,
						Tool: &agent.ToolCall{
							ID:   agent.ToolID(c.ID),
							Tool: c.Name,
							Hint: hint,
						},
					})
				}
			}
		case "user":
			// tool_result frames — ignored for Phase 1 (no approval loop).
		case "result":
			if env.Usage != nil {
				s.emit(agent.Event{
					Type:   agent.EvTokens,
					Tokens: &agent.Tokens{In: env.Usage.InputTokens, Out: env.Usage.OutputTokens},
				})
			}
			// `result.result` duplicates the final assistant text — we've
			// already emitted it as EvText fragments, don't re-emit here.
		default:
			// Unhandled envelope types (e.g. partial_assistant frames in
			// future claude-code versions) are silently ignored; stream-json
			// is forward-compatible.
		}
	}
	if err := sc.Err(); err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("stream read: %v", err)})
	}
}

// ─── helpers ────────────────────────────────────────────────────────────

// stderrCapture holds the last few stderr lines plus a "session busy" flag
// flipped when claude-code rejects a --resume because the session is locked
// by another live process. We can't probe the lock proactively (claude-code
// owns its own filesystem locks), so we observe the stderr surface instead.
type stderrCapture struct {
	mu              sync.Mutex
	lines           []string
	sessionBusy     bool
	sessionNotFound bool
}

// stderrSnapshot is the immutable view returned by stderrCapture.snapshot.
// Bundling the flags into a struct keeps the call site readable as we add
// more typed-error detections (currently SESSION_BUSY + SESSION_NOT_FOUND).
type stderrSnapshot struct {
	Lines           []string
	SessionBusy     bool
	SessionNotFound bool
}

const stderrCaptureMax = 16

func (c *stderrCapture) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) >= stderrCaptureMax {
		c.lines = c.lines[1:]
	}
	c.lines = append(c.lines, line)
	low := strings.ToLower(line)
	// Match a small set of phrases observed across claude-code versions when
	// resuming a session whose JSONL is currently held open by another
	// process. Better to false-positive than to silently fall back to "started
	// a new session" — the firmware will display the message verbatim so the
	// user can decide what to do (close the other claude / wait / pick a
	// different session).
	if strings.Contains(low, "session is currently in use") ||
		strings.Contains(low, "session is locked") ||
		strings.Contains(low, "session in use") ||
		strings.Contains(low, "session lock") ||
		strings.Contains(low, "could not acquire lock") ||
		strings.Contains(low, "another process is using") ||
		strings.Contains(low, "already running") {
		c.sessionBusy = true
	}
	// "No conversation found with session ID: <uuid>" — claude-code emits this
	// when --resume points at a deleted/missing conversation. Caller does
	// transparent retry by spawning a new CLI session (see ADR-014).
	if strings.Contains(low, "no conversation found with session id") {
		c.sessionNotFound = true
	}
}

func (c *stderrCapture) snapshot() stderrSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return stderrSnapshot{
		Lines:           out,
		SessionBusy:     c.sessionBusy,
		SessionNotFound: c.sessionNotFound,
	}
}

func drainStderr(r io.Reader, log *obs.Logger, sid agent.SessionID, cap *stderrCapture) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		log.Warnf("claude-code stderr sid=%s: %s", sid, line)
		if cap != nil {
			cap.add(line)
		}
	}
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	seen := make(map[string]bool, len(extra))
	for k := range extra {
		seen[k] = true
	}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			if seen[kv[:i]] {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// summarizeToolInput renders a short, human-readable hint from a tool_use
// input blob. Returns "" when no obvious field applies — the caller should
// treat an empty hint as "no preview available".
//
// Hint length is capped at 80 chars: long enough to recognise a command or
// file path at a glance on the playground / small device screen, short
// enough to avoid wrapping in tight UI slots.
func summarizeToolInput(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	switch name {
	case "Bash":
		return truncate(strings.TrimSpace(fields.Command), 80)
	case "Edit", "Write", "Read":
		return truncate(fields.FilePath, 80)
	default:
		return ""
	}
}
