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
	"syscall"
	"time"

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
	bin   string
	log   *obs.Logger
	extra []string
	env   map[string]string // driver-level env overrides (e.g. ANTHROPIC_BASE_URL)

	mu       sync.Mutex
	sessions map[agent.SessionID]*session

	// mcpFiles caches rendered --mcp-config files keyed by content hash so
	// butler sessions sharing the same dispatch server reuse one 0600 file
	// (ADR-024 §5). mcpMu guards it.
	mcpMu    sync.Mutex
	mcpFiles map[string]string
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
	// Env holds extra environment variables injected into every claude
	// subprocess. Keys here override the inherited process environment.
	// Intended for ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN overrides.
	Env map[string]string
	// Thinking enables Claude Code's extended thinking so the stream-json
	// carries `thinking` content blocks (surfaced as EvThinking for the admin
	// conversation page, ADR-029 §2.2). Implemented by injecting
	// `--settings {"alwaysThinkingEnabled":true}`; skipped when the operator
	// already supplied a --settings flag via ExtraArgs (last-flag wins).
	// Corresponds to AGENT_THINKING (default on).
	Thinking bool
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
	extra := append([]string(nil), opts.ExtraArgs...)
	// Fall back to the catalog's factory-default model (claudeCodeModels[0])
	// when the operator hasn't pinned a --model via AGENT_CLAUDE_CODE_EXTRA_ARGS,
	// so the runtime default and the device's model list stay in lockstep.
	if !hasFlag(extra, "--model") {
		if dm := defaultModelID(); dm != "" {
			extra = append(extra, "--model", dm)
		}
	}
	// Enable extended thinking so stream-json emits `thinking` content blocks
	// (ADR-029 §2.2). Skipped when the operator already pinned --settings via
	// ExtraArgs. Flows into the per-turn args through sessionFlags(d.extra).
	if opts.Thinking && !hasFlag(extra, "--settings") {
		extra = append(extra, "--settings", `{"alwaysThinkingEnabled":true}`)
	}
	driverEnv := make(map[string]string, len(opts.Env))
	for k, v := range opts.Env {
		driverEnv[k] = v
	}
	if baseURL, ok := driverEnv["ANTHROPIC_BASE_URL"]; ok {
		log.Infof("claude-code: ANTHROPIC_BASE_URL=%s", baseURL)
	}
	if _, ok := driverEnv["ANTHROPIC_AUTH_TOKEN"]; ok {
		log.Infof("claude-code: ANTHROPIC_AUTH_TOKEN=<set>")
	}
	return &Driver{
		bin:      bin,
		log:      log,
		extra:    extra,
		env:      driverEnv,
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
		// claude-code is the only butler-capable driver today: it honours
		// --append-system-prompt (persona) and --mcp-config (dispatch). See
		// ADR-023 §2.
		Butler: true,
	}
}

// Start allocates a new session. No subprocess is spawned here; the CLI is
// invoked on demand in Send so each turn can carry the latest --resume id.
//
// When opts.ResumeID is set, we use it AS the device-visible session id so
// the device's sessionId stays in lockstep with the on-disk JSONL filename.
// For new sessions we generate a plain UUID and pass it to the CLI via
// --session-id on the first turn, so the adapter id, CLI session id, and
// JSONL filename are all the same value from the start.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	var sid agent.SessionID
	if strings.TrimSpace(opts.ResumeID) != "" {
		sid = agent.SessionID(strings.TrimSpace(opts.ResumeID))
	} else {
		sid = agent.SessionID(uuid.NewString())
	}
	// Render the format-neutral MCP spec into claude's own --mcp-config JSON
	// file (ADR-024 §5). A render failure is non-fatal: the butler still runs,
	// just without dispatch this session.
	mcpConfig, err := d.renderMCPConfigFile(opts.MCPServers)
	if err != nil {
		d.log.Warnf("claudecode: render mcp-config failed: %v; session %s runs without dispatch", err, sid)
	}
	s := &session{
		id:           sid,
		events:       make(chan agent.Event, eventBufSize),
		resumeID:     opts.ResumeID,
		cwd:          opts.Cwd,
		env:          opts.Env,
		model:        strings.TrimSpace(opts.Model),
		systemPrompt: strings.TrimSpace(opts.SystemPrompt),
		mcpConfig:    mcpConfig,
		rootCtx:      ctx,
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

	// Determine session args. Priority:
	//   1. Session already has a resumeID from a prior turn → --resume.
	//   2. No resumeID yet → first turn: use --session-id so the CLI writes to
	//      the same UUID we already handed to the device. No TrimPrefix needed
	//      because adapter IDs are now plain UUIDs.
	if s.resumeID != "" {
		args = append(args, "--resume", s.resumeID)
	} else {
		// First turn, new session: tell the CLI which UUID to use so adapter
		// id == CLI session id == JSONL filename from the very first turn.
		args = append(args, "--session-id", string(sid))
		s.setResumeID(string(sid))
	}
	// Per-session flags (model override + system prompt) followed by the
	// driver/operator extra args. Extracted into a pure helper so it can be
	// unit-tested without spawning the CLI.
	args = append(args, s.sessionFlags(d.extra)...)

	ctx, cancel := context.WithCancel(s.rootCtx)
	cmd := exec.CommandContext(ctx, d.bin, args...)
	// Barge-in (ADR-028 §2.5.1): on ctx cancel send SIGTERM first so the CLI
	// gets a chance to flush its session JSONL, then SIGKILL after WaitDelay.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = s.cwd
	if len(d.env) > 0 || len(s.env) > 0 {
		base := mergeEnv(os.Environ(), d.env)
		cmd.Env = mergeEnv(base, s.env)
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
	s.interrupted = false
	s.mu.Unlock()

	d.log.Infof("claude-code: input sid=%s text=%q", sid, truncate(text, 200))
	d.log.Infof("claude-code: spawned sid=%s resume=%q model=%q pid=%d",
		sid, s.resumeID, s.model, cmd.Process.Pid)

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
	// Turn over: drop the cancel func so a late Interrupt() is a clean no-op
	// instead of marking the NEXT turn as interrupted.
	s.mu.Lock()
	s.cancel = nil
	s.mu.Unlock()
	if s.consumeInterrupted() {
		// Barge-in (ADR-028 §2.5.1): the turn was aborted on purpose. The
		// subprocess death is expected — suppress the exit error and tell the
		// device the turn was cancelled, then end the turn normally so the
		// NDJSON/WS stream tears down and the session stays resumable.
		d.log.Infof("claude-code: turn interrupted sid=%s resume=%q", sid, s.resumeID)
		s.emit(agent.Event{Type: agent.EvInterrupted})
	} else if waitErr != nil {
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

// CLISessionExists reports whether the on-disk JSONL transcript for the given
// CLI session id can be found under ~/.claude/projects/. Used by the agent
// proxy to skip a doomed --resume attempt when the conversation file has been
// deleted or GC'd by the CLI, avoiding the 4-7s cold-start penalty of
// spawning a process that will immediately fail with SESSION_NOT_FOUND.
func (d *Driver) CLISessionExists(cliSessionID string) bool {
	if strings.TrimSpace(cliSessionID) == "" {
		return false
	}
	path, err := d.findHistoryPath(cliSessionID)
	return err == nil && path != ""
}

// UpdateModel implements agent.ModelUpdater — lets the HTTP layer push a
// fresh model id into an existing session between turns so a mid-session
// device-side model switch takes effect on the next turn instead of waiting
// for the session to be evicted from the router's in-process cache.
func (d *Driver) UpdateModel(sid agent.SessionID, model string) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.setModel(strings.TrimSpace(model))
	return nil
}

// Interrupt aborts the in-flight turn's subprocess (SIGTERM → 2s grace →
// SIGKILL via cmd.Cancel/WaitDelay) while KEEPING the session and its
// resumeID, so the next Send still --resume's the same conversation.
// Implements agent.Interrupter (barge-in, ADR-028 §2.5.1). No-op when no
// turn is in flight.
func (d *Driver) Interrupt(sid agent.SessionID) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.mu.Lock()
	cancel := s.cancel
	if cancel != nil {
		s.interrupted = true
	}
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	d.log.Infof("claude-code: interrupt requested sid=%s", sid)
	cancel()
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
	close(s.events)
	return nil
}

// ─── session ────────────────────────────────────────────────────────────

// pendingDispatch tracks an in-flight mcp__bbclaw__dispatch tool call so we
// can correlate the tool_result back to the original tool_use.id.
type pendingDispatch struct {
	toolUseID string
	cwd       string
	title     string
	startedAt time.Time
}

type session struct {
	id           agent.SessionID
	events       chan agent.Event
	resumeID     string
	cwd          string
	env          map[string]string
	model        string // empty = use driver/operator default
	systemPrompt string // empty = no --append-system-prompt
	mcpConfig    string // empty = no --mcp-config (butler session only)
	rootCtx      context.Context

	seq uint64

	// pendingDispatches maps tool_use_id → pendingDispatch for mcp__bbclaw__
	// dispatch tool calls that have been started but not yet resolved. Keyed by
	// claude's tool_use.id (e.g. "toolu_01…"). Access is single-threaded within
	// parseStreamJSON so no mutex is needed.
	pendingDispatches map[string]*pendingDispatch

	mu     sync.Mutex
	cancel context.CancelFunc
	// interrupted marks the in-flight turn as deliberately aborted via
	// Interrupt() so Send suppresses the subprocess exit error and emits
	// EvInterrupted instead (ADR-028 §2.5.1). Reset at each turn start.
	interrupted bool
}

// consumeInterrupted reports whether the just-finished turn was aborted via
// Interrupt(), clearing the flag.
func (s *session) consumeInterrupted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.interrupted
	s.interrupted = false
	return v
}

// sessionFlags returns the per-session CLI flags appended after the
// resume/session-id args: the model override (StartOpts.Model), the system
// prompt (StartOpts.SystemPrompt → --append-system-prompt), then the
// driver/operator extra args. Pure (no side effects) so it is unit-testable.
//
// Model is placed before driverExtra so an operator-set --model in
// AGENT_CLAUDE_CODE_EXTRA_ARGS still wins (last-flag semantics), while the
// user's UI choice is honoured when no operator override is configured.
// --append-system-prompt is additive, so ordering is immaterial.
func (s *session) sessionFlags(driverExtra []string) []string {
	var out []string
	if s.model != "" {
		out = append(out, "--model", s.model)
	}
	if s.systemPrompt != "" {
		out = append(out, "--append-system-prompt", s.systemPrompt)
	}
	// --mcp-config wires the butler dispatch MCP server (ADR-021 §2). Only the
	// per-device butler session carries it; worker sessions leave it empty.
	if s.mcpConfig != "" {
		out = append(out, "--mcp-config", s.mcpConfig)
	}
	return append(out, driverExtra...)
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

func (s *session) setModel(m string) {
	s.mu.Lock()
	s.model = m
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
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"` // extended-thinking block payload (ADR-029 §2.2)
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"` // present in tool_result frames
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content (string or array)
}

type streamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func parseStreamJSON(r io.Reader, s *session, log *obs.Logger) {
	sc := bufio.NewScanner(r)
	// stream-json can emit long lines (large tool_result payloads).
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	// toolUseNames maps tool_use.id → tool_use.name so that tool_result frames
	// (in the "user" envelope) can look up which tool produced each result and
	// decide whether to emit EvDispatchStatus (ADR-021-firmware-ui §1.2).
	toolUseNames := make(map[string]string)
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
				s.emit(agent.Event{Type: agent.EvSessionInit, Text: env.SessionID})
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
						log.Infof("claude-code: reply sid=%s text=%q", s.id, truncate(c.Text, 200))
						s.emit(agent.Event{Type: agent.EvText, Text: c.Text})
					}
				case "thinking":
					// Extended-thinking block. Only present when thinking is
					// enabled (Options.Thinking → --settings alwaysThinkingEnabled,
					// ADR-029 §2.2). Surfaced as EvThinking so the admin
					// conversation page can render a collapsible thinking stream;
					// the device side only consumes the coarse turn.state phase.
					if c.Thinking != "" {
						log.Infof("claude-code: thinking sid=%s len=%d", s.id, len(c.Thinking))
						s.emit(agent.Event{Type: agent.EvThinking, Text: c.Thinking})
					}
				case "tool_use":
					if isMCPBBClawDispatch(c.Name) {
						// mcp__bbclaw__dispatch tool: emit EvDispatchStatus(started)
						// so butler.Engine can record it in the ring buffer and
						// the device can show "派发中: <cwd>…" in s_lbl_status.
						// (ADR-021-firmware-ui §1.2)
						toolUseNames[c.ID] = c.Name // track id→name for tool_result lookup
						cwd, title := parseDispatchInput(c.Input)
						log.Infof("claude-code: dispatch started sid=%s id=%s cwd=%q", s.id, c.ID, cwd)
						s.emit(agent.Event{
							Type: agent.EvDispatchStatus,
							Dispatch: &agent.DispatchStatus{
								Phase:  "started",
								TaskID: c.ID,
								Cwd:    cwd,
								Title:  title,
							},
						})
					} else {
						// All other tool_use frames: surface as EvToolCall (display-only).
						// Capabilities().ToolApproval stays false (Phase 2).
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
			}
		case "user":
			// tool_result frames: parse MCP bbclaw dispatch results and emit
			// EvDispatchStatus. All other tool_results remain ignored (Phase 1).
			if env.Message != nil {
				for _, c := range env.Message.Content {
					if c.Type == "tool_result" && isMCPBBClawDispatch(toolUseNames[c.ToolUseID]) {
						ds := parseDispatchResult(c.ToolUseID, c.Content)
						log.Infof("claude-code: dispatch result sid=%s id=%s phase=%s elapsed=%dms", s.id, ds.TaskID, ds.Phase, ds.ElapsedMs)
						s.emit(agent.Event{Type: agent.EvDispatchStatus, Dispatch: ds})
					}
				}
			}
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

// hasFlag reports whether args contains the given flag (e.g. "--model").
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
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

// ─── dispatch helpers (ADR-021-firmware-ui §1.2) ────────────────────────────

// mcpBBClawDispatchTool is the exact tool name that signals a butler dispatch.
// Only this tool emits EvDispatchStatus; other mcp__bbclaw__* tools (list_projects,
// task_status, task_result) continue to emit EvToolCall as usual, avoiding
// spurious "派发中…" labels for non-dispatch MCP calls.
const mcpBBClawDispatchTool = "mcp__bbclaw__dispatch"

// isMCPBBClawDispatch returns true iff name is exactly the dispatch tool.
func isMCPBBClawDispatch(name string) bool {
	return name == mcpBBClawDispatchTool
}

// parseDispatchInput extracts cwd and title from the dispatch tool_use input JSON.
// Returns empty strings on any parse error (non-fatal: ring buffer just has less info).
// Accepts both new schema (cwd/prompt) and legacy schema (project/task).
func parseDispatchInput(raw json.RawMessage) (cwd, title string) {
	if len(raw) == 0 {
		return "", ""
	}
	var in struct {
		Cwd     string `json:"cwd"`
		Prompt  string `json:"prompt"`
		Project string `json:"project"` // legacy field name
		Task    string `json:"task"`    // legacy field name
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", ""
	}
	cwd = strings.TrimSpace(in.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(in.Project)
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(in.Task)
	}
	return cwd, truncateCJK(prompt, 24)
}

// dispatchResultContent is the JSON shape the MCP dispatch tool returns.
type dispatchResultContent struct {
	Status         string `json:"status"` // "done" | "running" | "async" | "error"
	TaskID         string `json:"taskId"`
	ElapsedMs      int64  `json:"elapsedMs"`
	Error          string `json:"error"`
	ChildSessionID string `json:"childSessionId"` // worker session id for drill-down (ADR-029 §2.3)
}

// parseDispatchResult parses a tool_result content blob from the "user" envelope.
// toolUseID is the originating tool_use.id (used as TaskID fallback).
// content is json.RawMessage that may be a JSON string or a JSON array of content blocks.
func parseDispatchResult(toolUseID string, raw json.RawMessage) *agent.DispatchStatus {
	ds := &agent.DispatchStatus{TaskID: toolUseID, Phase: "done"}
	if len(raw) == 0 {
		return ds
	}

	// content can be a bare JSON string or an array like [{"type":"text","text":"..."}]
	var text string
	if raw[0] == '"' {
		_ = json.Unmarshal(raw, &text)
	} else if raw[0] == '[' {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					text = b.Text
					break
				}
			}
		}
	} else {
		// Try direct object parse
		text = string(raw)
	}

	// Try to parse the text as dispatchResultContent JSON
	var res dispatchResultContent
	if err := json.Unmarshal([]byte(text), &res); err == nil {
		if res.TaskID != "" {
			ds.TaskID = res.TaskID
		}
		if res.Status != "" {
			phase := res.Status
			if phase == "running" {
				phase = "async"
			}
			ds.Phase = phase
		}
		ds.ElapsedMs = res.ElapsedMs
		ds.ErrorMsg = res.Error
		ds.ChildSessionID = res.ChildSessionID
	}
	return ds
}

// truncateCJK truncates s to at most maxCJK CJK characters (or 2*maxCJK bytes
// for ASCII-only strings), appending "…" when truncation occurs.
func truncateCJK(s string, maxCJK int) string {
	runes := []rune(s)
	if len(runes) <= maxCJK {
		return s
	}
	return string(runes[:maxCJK]) + "…"
}
