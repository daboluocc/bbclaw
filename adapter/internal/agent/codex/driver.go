// Package codex implements agent.Driver by spawning the OpenAI `codex` CLI in
// non-interactive `exec --json` mode and translating its JSONL thread-event
// stream into the unified agent.Event stream (ADR-023).
//
// codex exec --json emits one JSON object per line, tagged by `type`:
//
//	{"type":"thread.started","thread_id":"<uuid>"}        — session id for resume
//	{"type":"turn.started"}
//	{"type":"item.started","item":{...}}
//	{"type":"item.updated","item":{...}}
//	{"type":"item.completed","item":{"type":"agent_message","text":"…"}}
//	{"type":"turn.completed","usage":{"input_tokens":N,"output_tokens":M}}
//	{"type":"turn.failed","error":{"message":"…"}}
//	{"type":"error","message":"…"}
//
// Multi-turn: the first Send runs `codex exec`, capturing thread_id; subsequent
// Sends run `codex exec resume <thread_id>` to continue the same conversation.
//
// NOTE (ADR-023): assistant text is emitted on item.completed only (final, not
// streamed token-by-token) to avoid double-emitting the item.updated partials.
// The exact assistant-message item shape was validated against codex-cli
// 0.122.0's documented schema; if a future codex build renames item_type/text,
// update mapItem accordingly.
package codex

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
	driverName     = "codex"
	defaultBin     = "codex"
	defaultTimeout = 5 * time.Minute
	eventBufSize   = 64
)

// Driver is the codex AgentDriver implementation.
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
	// Bin is the path to the `codex` binary; empty defaults to "codex"
	// resolved on PATH.
	Bin string
	// ExtraArgs appended after the fixed args. Do not include `exec`, `--json`,
	// `--skip-git-repo-check`, `--full-auto`, or `resume` — the driver sets
	// those itself.
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
		log.Warnf("codex: binary %q not on PATH, will use as-is (%v)", bin, err)
	} else {
		bin = resolved
		log.Infof("codex: resolved binary %q", bin)
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

// Capabilities implements agent.Driver. codex is not butler-capable: it does
// not honour --append-system-prompt / --mcp-config the way the butler needs.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ToolApproval:  false,
		Resume:        true,
		Streaming:     true,
		MaxInputBytes: 64 * 1024,
		// Butler-capable when opted in (ADR-024 §7): codex honours the persona
		// (-c model_instructions_file) and the dispatch MCP server
		// (-c mcp_servers.*), gated behind AGENT_CODEX_BUTLER_VERIFIED until the
		// operator confirms the model emits the dispatch call.
		Butler: butlerEnabled(),
	}
}

// Start allocates a new session. No subprocess is spawned here; the CLI is
// invoked on demand in Send so each turn can carry the latest thread id.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	sid := agent.SessionID("cx-" + uuid.NewString())
	s := &session{
		id:       sid,
		events:   make(chan agent.Event, eventBufSize),
		resumeID: opts.ResumeID,
		cwd:      opts.Cwd,
		env:      opts.Env,
		model:    strings.TrimSpace(opts.Model),
		rootCtx:  ctx,
	}
	// Butler persona + dispatch (ADR-024): render the format-neutral
	// SystemPrompt/MCPServers into codex -c overrides once per session. Non-fatal
	// — a render failure just drops butler features.
	if a, pf, pe, err := renderButlerArgs(strings.TrimSpace(opts.SystemPrompt), opts.MCPServers); err != nil {
		d.log.Warnf("codex: render butler args failed: %v; session %s runs without persona/dispatch", err, sid)
	} else {
		s.butlerArgs = a
		s.personaFile = pf
		s.procEnv = pe
		s.bypassApprovals = len(opts.MCPServers) > 0
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

// Send spawns `codex exec` (or `codex exec resume <id>`) for this turn and
// streams its JSONL output onto the session's event channel. Blocks until the
// subprocess exits or the per-turn timeout fires.
//
// --full-auto runs the model's shell commands in a workspace-write sandbox
// without approval prompts, which is required because the driver advertises
// ToolApproval=false and cannot serve interactive prompts.
func (d *Driver) Send(sid agent.SessionID, text string) (sendErr error) {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

	// On early exit the handler is blocked on <-events waiting for EvTurnEnd —
	// emit it so the handler can tear down cleanly.
	defer func() {
		if sendErr != nil {
			s.emit(agent.Event{Type: agent.EvError, Text: sendErr.Error()})
			s.emit(agent.Event{Type: agent.EvTurnEnd})
		}
	}()

	// codex reads the prompt as a positional arg. Resume continues the prior
	// thread; first turn starts a new one.
	//
	// Sandbox/approval flag: a butler session must invoke the dispatch MCP tool
	// non-interactively, but `--full-auto` auto-CANCELS MCP tool calls (no user
	// to approve → "user cancelled MCP tool call"). So butler sessions use
	// --dangerously-bypass-approvals-and-sandbox; worker/plain sessions keep
	// --full-auto (sandboxed shell auto-exec). Verified live 2026-06-10.
	sandboxFlag := "--full-auto"
	if s.bypassApprovals {
		sandboxFlag = "--dangerously-bypass-approvals-and-sandbox"
	}
	var args []string
	if s.resumeID != "" {
		args = []string{"exec", "resume", s.resumeID, "--json", "--skip-git-repo-check", sandboxFlag}
	} else {
		args = []string{"exec", "--json", "--skip-git-repo-check", sandboxFlag}
	}
	if s.model != "" {
		args = append(args, "--model", s.model)
	}
	// Butler persona + dispatch -c overrides (ADR-024), before driver extra args
	// and the prompt.
	args = append(args, s.butlerArgs...)
	args = append(args, d.extra...)
	args = append(args, text)

	// Per-turn timeout context derived from session root context so Stop() can
	// still cancel early.
	ctx, perTurnCancel := context.WithTimeout(s.rootCtx, d.timeout)
	defer perTurnCancel()

	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Dir = s.cwd
	// Merge per-session env with butler secret env (kept out of argv, inherited
	// by the spawned dispatch mcp-server).
	procEnv := s.env
	if len(s.procEnv) > 0 {
		procEnv = make(map[string]string, len(s.env)+len(s.procEnv))
		for k, v := range s.env {
			procEnv[k] = v
		}
		for k, v := range s.procEnv {
			procEnv[k] = v
		}
	}
	if len(procEnv) > 0 {
		cmd.Env = mergeEnv(os.Environ(), procEnv)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("codex: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex: start %s: %w", d.bin, err)
	}

	s.mu.Lock()
	s.cancel = perTurnCancel
	s.mu.Unlock()

	d.log.Infof("codex: input sid=%s text=%q", sid, truncate(text, 200))
	d.log.Infof("codex: spawned sid=%s resume=%q model=%q pid=%d timeout=%v cwd=%q",
		sid, s.resumeID, s.model, cmd.Process.Pid, d.timeout, s.cwd)

	go drainStderr(stderr, d.log, sid)

	parseStream(stdout, s, d.log)

	if err := cmd.Wait(); err != nil {
		msg := fmt.Sprintf("codex exit: %v", err)
		if ctx.Err() == context.DeadlineExceeded {
			msg = fmt.Sprintf("codex timed out after %v", d.timeout)
		}
		s.emit(agent.Event{Type: agent.EvError, Text: msg})
	}
	s.emit(agent.Event{Type: agent.EvTurnEnd})
	return nil
}

// Approve is not supported; returns ErrUnsupported per Capabilities.
func (d *Driver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return agent.ErrUnsupported
}

// UpdateModel implements agent.ModelUpdater — codex's CLI accepts a different
// --model each invocation, so mid-session switches just apply on the next turn.
func (d *Driver) UpdateModel(sid agent.SessionID, model string) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.mu.Lock()
	s.model = strings.TrimSpace(model)
	s.mu.Unlock()
	return nil
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
	if s.personaFile != "" {
		_ = os.Remove(s.personaFile)
	}
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
	model    string // empty = driver/operator default
	rootCtx  context.Context

	// Butler config (ADR-024): -c override args (persona + dispatch), the
	// persona temp file to remove on Stop, and secret env routed via the codex
	// process env (kept out of argv). All empty for non-butler sessions.
	butlerArgs      []string
	personaFile     string
	procEnv         map[string]string
	bypassApprovals bool // butler session: bypass approvals so MCP dispatch executes

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

// ─── JSONL parser ───────────────────────────────────────────────────────

type codexEvent struct {
	Type     string      `json:"type"`
	ThreadID string      `json:"thread_id,omitempty"`
	Message  string      `json:"message,omitempty"`
	Item     *codexItem  `json:"item,omitempty"`
	Error    *codexError `json:"error,omitempty"`
	Usage    *codexUsage `json:"usage,omitempty"`
}

type codexItem struct {
	// codex tags the item kind as item_type; older builds used `type`. We read
	// both and prefer item_type.
	ItemType string `json:"item_type,omitempty"`
	Type     string `json:"type,omitempty"`
	Text     string `json:"text,omitempty"`
	Command  string `json:"command,omitempty"`
}

func (it *codexItem) kind() string {
	if it.ItemType != "" {
		return it.ItemType
	}
	return it.Type
}

type codexError struct {
	Message string `json:"message,omitempty"`
}

type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func parseStream(r io.Reader, s *session, log *obs.Logger) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.Warnf("codex: unparseable line sid=%s err=%v line=%q", s.id, err, truncate(string(line), 200))
			continue
		}

		switch ev.Type {
		case "thread.started":
			// thread_id is the resume handle. Record it for the next turn and
			// surface it so the logical-session manager can map logical id →
			// CLI session id (mirrors claudecode's EvSessionInit).
			if ev.ThreadID != "" && s.resumeID == "" {
				s.setResumeID(ev.ThreadID)
				log.Infof("codex: session init sid=%s thread=%s", s.id, ev.ThreadID)
				s.emit(agent.Event{Type: agent.EvSessionInit, Text: ev.ThreadID})
			}

		case "item.completed":
			mapItem(ev.Item, s, log)

		case "turn.completed":
			if ev.Usage != nil {
				s.emit(agent.Event{
					Type:   agent.EvTokens,
					Tokens: &agent.Tokens{In: ev.Usage.InputTokens, Out: ev.Usage.OutputTokens},
				})
			}
			// turn.completed does NOT emit EvTurnEnd — that is emitted in Send
			// after cmd.Wait() so it always fires, even on a crash.

		case "error":
			if ev.Message != "" {
				s.emit(agent.Event{Type: agent.EvError, Text: codexErrText(ev.Message)})
			}

		case "turn.failed":
			if ev.Error != nil && ev.Error.Message != "" {
				s.emit(agent.Event{Type: agent.EvError, Text: codexErrText(ev.Error.Message)})
			}

		default:
			// item.started / item.updated / turn.started and unknown types are
			// silently ignored (text is emitted on item.completed only).
		}
	}
	if err := sc.Err(); err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("stream read: %v", err)})
	}
}

// mapItem translates a completed codex item into an agent.Event. Assistant
// messages become EvText; command executions become a display-only EvToolCall.
func mapItem(it *codexItem, s *session, log *obs.Logger) {
	if it == nil {
		return
	}
	switch it.kind() {
	case "agent_message", "assistant_message":
		// codex-cli 0.122 emits item.type:"agent_message" for the final
		// assistant text (verified live 2026-06-10); accept the older
		// "assistant_message" spelling too for forward/backward safety.
		if it.Text != "" {
			log.Infof("codex: reply sid=%s text=%q", s.id, truncate(it.Text, 200))
			s.emit(agent.Event{Type: agent.EvText, Text: it.Text})
		}
	case "command_execution":
		if it.Command != "" {
			log.Infof("codex: command sid=%s cmd=%q", s.id, truncate(it.Command, 200))
			s.emit(agent.Event{
				Type: agent.EvToolCall,
				Tool: &agent.ToolCall{Tool: "Bash", Hint: truncate(it.Command, 120)},
			})
		}
	default:
		// reasoning / file_change / mcp_tool_call / web_search etc. are not
		// surfaced to the device for now.
	}
}

// codexErrText unwraps codex's nested JSON error blob into a human-readable
// message when possible. codex emits `message` as a JSON string that itself
// contains {"error":{"message":"…"}}; surface the inner message when present.
func codexErrText(raw string) string {
	var nested struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &nested); err == nil && nested.Error.Message != "" {
		return nested.Error.Message
	}
	return raw
}

// ─── helpers ────────────────────────────────────────────────────────────

func drainStderr(r io.Reader, log *obs.Logger, sid agent.SessionID) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Warnf("codex stderr sid=%s: %s", sid, sc.Text())
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
