// Package opencode implements agent.Driver by spawning the `opencode` CLI
// in `run --format json` mode and translating its NDJSON output into the
// unified agent.Event stream.
//
// OpenCode run JSON event types:
//
//	step_start  — new agent step; sessionID captured for resume
//	text         — assistant text fragment (→ EvText)
//	reasoning    — thinking block (logged, not emitted)
//	tool_use     — tool invocation (→ EvToolCall, display-only)
//	step_finish  — end of step; tokens from part.tokens (→ EvTokens)
//
// Multi-turn: first Send spawns `opencode run`, subsequent Send passes
// --session <id> to resume the same conversation.
package opencode

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
	"time"

	"github.com/google/uuid"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

const (
	driverName    = "opencode"
	defaultBin    = "opencode"
	defaultTimeout = 5 * time.Minute
	eventBufSize  = 64
)

// Driver is the opencode AgentDriver implementation.
type Driver struct {
	bin     string
	timeout time.Duration
	log     *obs.Logger
	extra   []string

	mu       sync.Mutex
	sessions map[agent.SessionID]*session
}

// Options configures the driver.
type Options struct {
	// Bin is the path to the `opencode` binary; empty defaults to "opencode"
	// resolved on PATH.
	Bin string
	// ExtraArgs appended after the fixed args (e.g. "--model",
	// "opencode/deepseek-v4-pro"). Do not include `run`, `--format json`,
	// `--print-logs`, `--thinking`, or `--dangerously-skip-permissions` —
	// the driver sets those itself.
	ExtraArgs []string
	// Timeout is the per-turn deadline. Zero means defaultTimeout (5 min).
	Timeout time.Duration
}

// New constructs a Driver. The logger is required.
func New(opts Options, log *obs.Logger) *Driver {
	bin := strings.TrimSpace(opts.Bin)
	if bin == "" {
		bin = defaultBin
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		log.Warnf("opencode: binary %q not on PATH, will use as-is (%v)", bin, err)
	} else {
		bin = resolved
		log.Infof("opencode: resolved binary %q", bin)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Driver{
		bin:      bin,
		timeout:  timeout,
		log:      log,
		extra:    append([]string(nil), opts.ExtraArgs...),
		sessions: make(map[agent.SessionID]*session),
	}
}

// Name implements agent.Driver.
func (d *Driver) Name() string { return driverName }

// Capabilities implements agent.Driver.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ToolApproval:  false,
		Resume:        true,
		Streaming:     true,
		MaxInputBytes: 64 * 1024,
	}
}

// Start allocates a new session. No subprocess is spawned here; the CLI is
// invoked on demand in Send so each turn can carry the latest --session id.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	sid := agent.SessionID("oc-" + uuid.NewString())
	s := &session{
		id:       sid,
		events:   make(chan agent.Event, eventBufSize),
		resumeID: opts.ResumeID,
		cwd:      opts.Cwd,
		env:      opts.Env,
		rootCtx:  ctx,
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

// Send spawns `opencode run` for this turn and streams its NDJSON output
// onto the session's event channel. Blocks until the subprocess exits or
// the per-turn timeout fires.
//
// --dangerously-skip-permissions is always passed because the driver
// advertises ToolApproval=false and cannot serve approval prompts.
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

	args := []string{"run", "--format", "json", "--print-logs", "--thinking", "--dangerously-skip-permissions"}
	if s.resumeID != "" {
		args = append(args, "--session", s.resumeID)
	}
	args = append(args, text)
	args = append(args, d.extra...)

	// Per-turn timeout context derived from session root context so Stop()
	// can still cancel early.
	ctx, perTurnCancel := context.WithTimeout(s.rootCtx, d.timeout)
	defer perTurnCancel()

	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Dir = s.cwd
	if len(s.env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), s.env)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencode: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("opencode: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencode: start %s: %w", d.bin, err)
	}

	s.mu.Lock()
	s.cancel = perTurnCancel
	s.mu.Unlock()

	d.log.Infof("opencode: spawned sid=%s resume=%q pid=%d timeout=%v", sid, s.resumeID, cmd.Process.Pid, d.timeout)

	go drainStderr(stderr, d.log, sid)

	parseStream(stdout, s, d.log)

	if err := cmd.Wait(); err != nil {
		msg := fmt.Sprintf("opencode exit: %v", err)
		if ctx.Err() == context.DeadlineExceeded {
			msg = fmt.Sprintf("opencode timed out after %v", d.timeout)
		}
		s.emit(agent.Event{Type: agent.EvError, Text: msg})
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

	seq atomic.Uint64

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (s *session) emit(e agent.Event) {
	e.Seq = s.seq.Add(1)
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

// ─── NDJSON parser ──────────────────────────────────────────────────────

// opencode run --format json event schema:
//
//   {"type":"step_start", "sessionID":"ses_...", "part":{...}}
//   {"type":"reasoning", "sessionID":"...", "part":{"type":"reasoning","text":"..."}}
//   {"type":"text",       "sessionID":"...", "part":{"type":"text","text":"..."}}
//   {"type":"tool_use",   "sessionID":"...", "part":{"type":"tool","tool":"Bash",...}}
//   {"type":"step_finish","sessionID":"...", "part":{"type":"step-finish","tokens":{...}}}
//
// We emit:
//   - EvText for each text block
//   - EvToolCall for each tool_use (display-only; ToolApproval=false)
//   - EvTokens on step_finish tokens
//   - EvError for unparseable lines

type opencodeEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID,omitempty"`
	Part      *opencodePart   `json:"part,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type opencodePart struct {
	Type    string      `json:"type"`
	Text    string      `json:"text,omitempty"`
	Tool    string      `json:"tool,omitempty"`
	CallID  string      `json:"callID,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	State   *toolState  `json:"state,omitempty"`
	Tokens  *tokenUsage `json:"tokens,omitempty"`
}

type toolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

type tokenUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

func parseStream(r io.Reader, s *session, log *obs.Logger) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev opencodeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.Warnf("opencode: unparseable line sid=%s err=%v line=%q", s.id, err, truncate(string(line), 200))
			continue
		}

		switch ev.Type {
		case "step_start":
			if ev.SessionID != "" && s.resumeID == "" {
				s.setResumeID(ev.SessionID)
				log.Infof("opencode: session init sid=%s cli_session=%s", s.id, ev.SessionID)
			}

		case "text":
			if ev.Part != nil && ev.Part.Text != "" {
				s.emit(agent.Event{Type: agent.EvText, Text: ev.Part.Text})
			}

		case "reasoning":
			if ev.Part != nil && ev.Part.Text != "" {
				log.Infof("opencode: reasoning sid=%s preview=%q", s.id, truncate(ev.Part.Text, 80))
			}

		case "tool_use":
			if ev.Part != nil {
				hint := summarizeToolInput(ev.Part.Tool, ev.Part.State)
				log.Infof("opencode: tool_use sid=%s tool=%s callID=%s hint=%q", s.id, ev.Part.Tool, ev.Part.CallID, hint)
				s.emit(agent.Event{
					Type: agent.EvToolCall,
					Tool: &agent.ToolCall{
						ID:   agent.ToolID(ev.Part.CallID),
						Tool: ev.Part.Tool,
						Hint: hint,
					},
				})
			}

		case "step_finish":
			if ev.Part != nil && ev.Part.Tokens != nil {
				s.emit(agent.Event{
					Type:   agent.EvTokens,
					Tokens: &agent.Tokens{In: ev.Part.Tokens.Input, Out: ev.Part.Tokens.Output},
				})
			}
			// step_finish does NOT emit EvTurnEnd — the subprocess may
			// spawn more steps (reason="tool-calls"). EvTurnEnd is emitted
			// in Send after cmd.Wait().

		default:
			// Unhandled event types silently ignored.
		}
	}
	if err := sc.Err(); err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("stream read: %v", err)})
	}
}

// ─── helpers ────────────────────────────────────────────────────────────

func drainStderr(r io.Reader, log *obs.Logger, sid agent.SessionID) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Warnf("opencode stderr sid=%s: %s", sid, sc.Text())
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
	return s[:n] + "..."
}

// summarizeToolInput renders a short, human-readable hint from a tool_use
// state.input blob. Returns "" when no obvious field applies.
//
// opencode tool names: Bash, Read, Write, Edit, Glob, Grep, etc.
// Hint length capped at 80 chars.
func summarizeToolInput(tool string, state *toolState) string {
	if state == nil || len(state.Input) == 0 {
		return ""
	}
	var fields struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
	}
	if err := json.Unmarshal(state.Input, &fields); err != nil {
		return ""
	}
	switch tool {
	case "Bash":
		return truncate(strings.TrimSpace(fields.Command), 80)
	case "Read", "Edit", "Write":
		return truncate(fields.FilePath, 80)
	case "Glob", "Grep":
		return truncate(fields.Pattern, 80)
	default:
		return ""
	}
}
