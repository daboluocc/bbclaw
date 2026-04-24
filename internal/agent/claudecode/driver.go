// Package claudecode implements agent.Driver by spawning the `claude` CLI
// in `-p --output-format stream-json --verbose` mode and translating its
// NDJSON output into the unified agent.Event stream.
//
// Phase 1 scope (see design/agent_bus.md):
//   - one-shot per Send: spawn a fresh subprocess, carry session continuity
//     via --resume using the session_id Claude emits in its init event
//   - emit EvText for assistant text blocks, EvTurnEnd on result, EvError
//     on failures. Tool-use events are logged but not yet surfaced as
//     EvToolCall — approval plumbing lands in a later phase.
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
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	sid := agent.SessionID("cc-" + uuid.NewString())
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
func (d *Driver) Send(sid agent.SessionID, text string) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

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

	// Drain stderr into the log (don't fail on stderr noise).
	go drainStderr(stderr, d.log, sid)

	// Parse stdout stream-json, emitting events.
	parseStreamJSON(stdout, s, d.log)

	if err := cmd.Wait(); err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("claude-code exit: %v", err)})
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
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
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
					log.Infof("claude-code: tool_use sid=%s tool=%s id=%s (EvToolCall not yet wired)", s.id, c.Name, c.ID)
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

func drainStderr(r io.Reader, log *obs.Logger, sid agent.SessionID) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Warnf("claude-code stderr sid=%s: %s", sid, sc.Text())
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
