// Package claudecode implements agent.Driver on top of the agent-runner SDK
// (github.com/zhoushoujianwork/agent-runner): each Send runs one headless
// claude turn through runner.Runner (stream-json in/out, prompt via stdin —
// never argv) and translates the normalized runner events into the unified
// agent.Event stream.
//
// Scope (see design/agent_bus.md):
//   - one-shot per Send: agent-runner spawns a fresh subprocess per turn,
//     session continuity via --resume using the session_id Claude emits in
//     its init event
//   - emit EvText for assistant text blocks, EvTurnEnd on completion, EvError
//     on failures. tool_use frames are surfaced as EvToolCall *display-only*
//     events — Capabilities().ToolApproval stays false until the approval
//     round-trip is wired onto agent-runner's OnPermission callback.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	claudeengine "github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/executor/host"
	agentrunner "github.com/zhoushoujianwork/agent-runner/runner"
)

const (
	driverName   = "claude-code"
	defaultBin   = "claude"
	eventBufSize = 64
)

// Driver is the claude-code AgentDriver implementation.
type Driver struct {
	bin    string
	runner *agentrunner.Runner
	log    *obs.Logger
	extra  []string
	env    map[string]string // driver-level env overrides (e.g. ANTHROPIC_BASE_URL)

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
	// the runner engine sets those itself.
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
	// ExtraArgs. Flows into every turn through buildRequest's ExtraArgs.
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
		runner:   &agentrunner.Runner{Engine: claudeengine.New(bin), Executor: host.New()},
		log:      log,
		extra:    extra,
		env:      driverEnv,
		sessions: make(map[agent.SessionID]*session),
	}
}

// Name implements agent.Driver.
func (d *Driver) Name() string { return driverName }

// Capabilities implements agent.Driver: streaming and resume work, tool
// approval is not yet plumbed onto agent-runner's OnPermission callback.
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
		seenToolUse:  make(map[string]bool),
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

// Send runs one claude turn through agent-runner and streams its normalized
// events onto the session's event channel. Blocks until the turn completes
// (caller should invoke Send in a goroutine if they want to keep reading
// Events concurrently; events are buffered anyway).
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

	req := buildRequest(s, text, d.extra, mergeEnvMaps(d.env, s.env))
	if req.NewSessionID != "" {
		// First turn, new session: the CLI was told which UUID to use so
		// adapter id == CLI session id == JSONL filename from the very start.
		s.setResumeID(req.NewSessionID)
	}

	ctx, cancel := context.WithCancel(s.rootCtx)
	defer cancel()
	handle, err := d.runner.Run(ctx, req)
	if err != nil {
		return fmt.Errorf("claude-code: start %s: %w", d.bin, err)
	}

	s.mu.Lock()
	s.cancel = cancel
	s.interrupted = false
	s.mu.Unlock()

	d.log.Infof("claude-code: input sid=%s text=%q", sid, truncate(text, 200))
	d.log.Infof("claude-code: spawned sid=%s resume=%q session=%q model=%q",
		sid, req.SessionID, req.NewSessionID, s.model)

	// Capture stderr diagnostics while logging them: if claude-code refuses to
	// resume a locked session, we want to surface SESSION_BUSY to the device
	// rather than a bare exit error.
	stderrCap := &stderrCapture{}
	// toolUseNames maps tool_use.id → tool_use.name so tool_result events can
	// look up which tool produced each result and decide whether to emit
	// EvDispatchStatus (ADR-021-firmware-ui §1.2).
	toolUseNames := make(map[string]string)
	for ev := range handle.Events() {
		d.consumeRunnerEvent(s, stderrCap, toolUseNames, ev)
	}
	_, waitErr := handle.Wait()

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

// buildRequest maps one turn onto an agent-runner request. Session continuity:
// a session that already has a resumeID resumes it; otherwise the adapter's
// own UUID becomes the CLI session id via NewSessionID. Pure (no side
// effects) so it is unit-testable without spawning the CLI.
func buildRequest(s *session, text string, driverExtra []string, env map[string]string) agentrunner.Request {
	req := agentrunner.Request{
		Prompt:             text,
		WorkDir:            s.cwd,
		Model:              s.currentModel(),
		AppendSystemPrompt: s.systemPrompt,
		MCPConfig:          s.mcpConfig,
		Env:                env,
		ExtraArgs:          append([]string(nil), driverExtra...),
	}
	if s.resumeID != "" {
		req.SessionID = s.resumeID
	} else {
		req.NewSessionID = string(s.id)
	}
	return req
}

// consumeRunnerEvent translates one normalized agent-runner event into the
// unified agent.Event stream, preserving the exact semantics of the old
// in-process stream-json parser. Events that originate from stream_event
// partial frames are dropped via frameMeta so text/thinking/tool blocks are
// surfaced exactly once (from their full assistant/user frame).
func (d *Driver) consumeRunnerEvent(s *session, cap *stderrCapture, toolUseNames map[string]string, ev agentrunner.Event) {
	switch ev.Type {
	case agentrunner.EventDiagnostic:
		d.log.Warnf("claude-code stderr sid=%s: %s", s.id, ev.Text)
		cap.add(ev.Text)

	case agentrunner.EventInit:
		if _, subtype := frameMeta(ev.Raw); subtype == "init" && ev.SessionID != "" {
			s.setResumeID(ev.SessionID)
			s.emit(agent.Event{Type: agent.EvSessionInit, Text: ev.SessionID})
			d.log.Infof("claude-code: session init sid=%s cli_session=%s", s.id, ev.SessionID)
		}

	case agentrunner.EventText:
		if frameType, _ := frameMeta(ev.Raw); frameType != "assistant" {
			return
		}
		if ev.Text != "" {
			d.log.Infof("claude-code: reply sid=%s text=%q", s.id, truncate(ev.Text, 200))
			s.emit(agent.Event{Type: agent.EvText, Text: ev.Text})
		}

	case agentrunner.EventThinking:
		// Extended-thinking block. Only present when thinking is enabled
		// (Options.Thinking → --settings alwaysThinkingEnabled, ADR-029 §2.2).
		// Surfaced as EvThinking so the admin conversation page can render a
		// collapsible thinking stream; the device side only consumes the
		// coarse turn.state phase.
		if frameType, _ := frameMeta(ev.Raw); frameType != "assistant" {
			return
		}
		if ev.Text != "" {
			d.log.Infof("claude-code: thinking sid=%s len=%d", s.id, len(ev.Text))
			s.emit(agent.Event{Type: agent.EvThinking, Text: ev.Text})
		}

	case agentrunner.EventToolUse:
		if frameType, _ := frameMeta(ev.Raw); frameType != "assistant" {
			return
		}
		if ev.Tool == nil {
			return
		}
		if isMCPBBClawDispatch(ev.Tool.Name) {
			// mcp__bbclaw__dispatch tool: emit EvDispatchStatus(started)
			// so butler.Engine can record it in the ring buffer and
			// the device can show "派发中: <cwd>…" in s_lbl_status.
			// (ADR-021-firmware-ui §1.2)
			toolUseNames[ev.Tool.ID] = ev.Tool.Name // track id→name for tool_result lookup
			cwd, title := parseDispatchInput(ev.Tool.Input)
			d.log.Infof("claude-code: dispatch started sid=%s id=%s cwd=%q", s.id, ev.Tool.ID, cwd)
			s.emit(agent.Event{
				Type: agent.EvDispatchStatus,
				Dispatch: &agent.DispatchStatus{
					Phase:  "started",
					TaskID: ev.Tool.ID,
					Cwd:    cwd,
					Title:  title,
				},
			})
		} else if ev.Tool.ID != "" && s.seenToolUse[ev.Tool.ID] {
			// Already surfaced this exact tool_use earlier in the session —
			// a replayed/duplicate frame (e.g. a --resume that re-streams the
			// prior conversation). Drop it so the device doesn't re-paint a
			// stale grey tool chip (e.g. a greeting "replaying" an earlier
			// turn's door-control / set-volume calls).
			d.log.Infof("claude-code: tool_use dup-skip sid=%s tool=%s id=%s", s.id, ev.Tool.Name, ev.Tool.ID)
		} else {
			// All other tool_use frames: surface as EvToolCall (display-only).
			// Capabilities().ToolApproval stays false.
			if ev.Tool.ID != "" {
				s.seenToolUse[ev.Tool.ID] = true
			}
			hint := summarizeToolInput(ev.Tool.Name, ev.Tool.Input)
			d.log.Infof("claude-code: tool_use sid=%s tool=%s id=%s hint=%q", s.id, ev.Tool.Name, ev.Tool.ID, hint)
			s.emit(agent.Event{
				Type: agent.EvToolCall,
				Tool: &agent.ToolCall{
					ID:   agent.ToolID(ev.Tool.ID),
					Tool: ev.Tool.Name,
					Hint: hint,
				},
			})
		}

	case agentrunner.EventToolResult:
		// tool_result frames: parse MCP bbclaw dispatch results and emit
		// EvDispatchStatus. All other tool_results remain ignored.
		if frameType, _ := frameMeta(ev.Raw); frameType != "user" {
			return
		}
		if ev.Tool == nil {
			return
		}
		if isMCPBBClawDispatch(toolUseNames[ev.Tool.ToolUseID]) {
			ds := parseDispatchResult(ev.Tool.ToolUseID, ev.Tool.Content)
			d.log.Infof("claude-code: dispatch result sid=%s id=%s phase=%s elapsed=%dms", s.id, ds.TaskID, ds.Phase, ds.ElapsedMs)
			s.emit(agent.Event{Type: agent.EvDispatchStatus, Dispatch: ds})
		}

	case agentrunner.EventUsage:
		if ev.Usage != nil {
			s.emit(agent.Event{
				Type:   agent.EvTokens,
				Tokens: &agent.Tokens{In: int(ev.Usage.InputTokens), Out: int(ev.Usage.OutputTokens)},
			})
		}

	default:
		// EventTextDelta / EventResult / EventRaw carry no additional
		// information for the device stream: deltas duplicate the full text
		// blocks and the result aggregate is consumed via handle.Wait.
	}
}

// frameMeta extracts the type/subtype of the original stream-json frame an
// event was parsed from, so duplicate partial frames (stream_event) can be
// filtered out.
func frameMeta(raw json.RawMessage) (frameType, subtype string) {
	if len(raw) == 0 {
		return "", ""
	}
	var f struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(raw, &f)
	return f.Type, f.Subtype
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

// Interrupt aborts the in-flight turn (agent-runner cancels the run: SIGTERM
// → grace → SIGKILL through the host executor) while KEEPING the session and
// its resumeID, so the next Send still --resume's the same conversation.
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
	// the Send event loop so no mutex is needed.
	pendingDispatches map[string]*pendingDispatch

	// seenToolUse remembers tool_use.id values already surfaced as EvToolCall so
	// a given tool call is shown to the device at most once for the life of the
	// session. claude's tool_use ids are stable + unique per call, so on a turn
	// that re-emits prior tool_use blocks (e.g. a `--resume` that replays the
	// conversation) the repeats are dropped instead of re-painting stale grey
	// tool chips on the device — the symptom seen when a plain greeting "replayed"
	// a previous turn's door-control / set-volume calls. Single-threaded within
	// the Send event loop; persists across turns because the session is reused.
	seenToolUse map[string]bool

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

func (s *session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// ─── helpers ────────────────────────────────────────────────────────────

// stderrCapture holds the last few stderr lines plus a "session busy" flag
// flipped when claude-code rejects a --resume because the session is locked
// by another live process. We can't probe the lock proactively (claude-code
// owns its own filesystem locks), so we observe the stderr surface (agent-
// runner's diagnostic events) instead.
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

// mergeEnvMaps merges driver-level env overrides with per-session ones; the
// session wins on conflicts. Returns nil when both are empty so agent-runner
// simply inherits the process environment.
func mergeEnvMaps(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
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
// Hint length is capped at toolHintMaxLen chars: enough that the device prints
// the (near-)complete command/path to its serial conversation log and shows it
// in the transcript, while still bounding pathological inputs (long heredocs).
// The web playground/admin have room to wrap; the device transcript tool line
// is dimmed/secondary so a 2-3 line wrap is acceptable.
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
		return truncate(strings.TrimSpace(fields.Command), toolHintMaxLen)
	case "Edit", "Write", "Read":
		return truncate(fields.FilePath, toolHintMaxLen)
	default:
		return ""
	}
}

// toolHintMaxLen bounds the tool_call hint sent to the device (ADR-030). The
// firmware reception buffer (bb_adapter_client.c char hint[256]) must stay >=
// this + the "…" suffix.
const toolHintMaxLen = 240

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
