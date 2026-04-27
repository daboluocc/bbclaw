// Package aider implements agent.Driver by spawning the `aider` CLI
// (https://aider.chat) in non-interactive mode and returning its plain-text
// reply on the unified agent.Event stream.
//
// Unlike opencode/claudecode this driver does not consume NDJSON: aider
// emits the assistant turn as raw stdout text. We capture stdout in full,
// emit one EvText per non-empty line (so the chat overlay can stream),
// then EvTurnEnd after cmd.Wait().
//
// Multi-turn continuity is achieved by passing the same chat history file
// across calls (--chat-history-file <path>). The path is created lazily
// per session in $TMPDIR/bbclaw-aider-<sessionID>.md and reused on every
// Send. Aider reads previous turns from this file and appends new ones.
package aider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

const (
	driverName     = "aider"
	defaultBin     = "aider"
	defaultTimeout = 5 * time.Minute
	eventBufSize   = 64
)

// Driver is the aider AgentDriver implementation.
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
	// Bin is the path to the `aider` binary; empty defaults to "aider"
	// resolved on PATH.
	Bin string
	// ExtraArgs appended after the fixed args (e.g. "--model",
	// "openrouter/anthropic/claude-sonnet-4"). Do not include
	// --message / --yes-always / --no-pretty / --no-stream /
	// --no-auto-commits / --no-git / --chat-history-file — the driver
	// sets those itself.
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
		log.Warnf("aider: binary %q not on PATH, will use as-is (%v)", bin, err)
	} else {
		bin = resolved
		log.Infof("aider: resolved binary %q", bin)
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
//
// Aider auto-applies tool changes (file edits, git commits when enabled)
// without asking, so ToolApproval is false. Resume is true via the
// per-session chat-history file. Streaming is false because we emit
// stdout as one batch (line-split) at the end of the turn — aider's own
// streaming UI is captured into a single buffer.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ToolApproval:  false,
		Resume:        true,
		Streaming:     false,
		MaxInputBytes: 64 * 1024,
	}
}

// Start allocates a new session and prepares its chat history file. No
// subprocess is spawned here; the CLI is invoked on demand in Send.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	sid := agent.SessionID("ai-" + uuid.NewString())
	historyPath := filepath.Join(os.TempDir(), "bbclaw-aider-"+string(sid)+".md")
	s := &session{
		id:          sid,
		events:      make(chan agent.Event, eventBufSize),
		resumeID:    opts.ResumeID,
		cwd:         opts.Cwd,
		env:         opts.Env,
		rootCtx:     ctx,
		historyPath: historyPath,
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

// Send spawns `aider --message <text>` for this turn. Aider reads previous
// context from --chat-history-file and appends the new turn to the same
// file, so subsequent Sends carry the full conversation.
//
// --yes-always and --no-auto-commits prevent any interactive prompts; the
// driver advertises ToolApproval=false and cannot serve approval round-trips.
func (d *Driver) Send(sid agent.SessionID, text string) (sendErr error) {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

	defer func() {
		if sendErr != nil {
			s.emit(agent.Event{Type: agent.EvError, Text: sendErr.Error()})
			s.emit(agent.Event{Type: agent.EvTurnEnd})
		}
	}()

	args := []string{
		"--message", text,
		"--yes-always",
		"--no-pretty",
		"--no-stream",
		"--no-auto-commits",
		"--no-git",
		"--no-show-model-warnings",
		"--chat-history-file", s.historyPath,
	}
	args = append(args, d.extra...)

	ctx, perTurnCancel := context.WithTimeout(s.rootCtx, d.timeout)
	defer perTurnCancel()

	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Dir = s.cwd
	if len(s.env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), s.env)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("aider: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("aider: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("aider: start %s: %w", d.bin, err)
	}

	s.mu.Lock()
	s.cancel = perTurnCancel
	s.mu.Unlock()

	d.log.Infof("aider: spawned sid=%s pid=%d timeout=%v history=%s",
		sid, cmd.Process.Pid, d.timeout, s.historyPath)

	go drainStderr(stderr, d.log, sid)

	parseStdout(stdout, s, d.log)

	if err := cmd.Wait(); err != nil {
		msg := fmt.Sprintf("aider exit: %v", err)
		if ctx.Err() == context.DeadlineExceeded {
			msg = fmt.Sprintf("aider timed out after %v", d.timeout)
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

// Stop terminates any in-flight subprocess, removes the chat history file,
// and closes the session.
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
	if s.historyPath != "" {
		_ = os.Remove(s.historyPath)
	}
	close(s.events)
	return nil
}

// ─── session ────────────────────────────────────────────────────────────

type session struct {
	id          agent.SessionID
	events      chan agent.Event
	resumeID    string
	cwd         string
	env         map[string]string
	rootCtx     context.Context
	historyPath string

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

// ─── stdout parser ──────────────────────────────────────────────────────

// parseStdout reads aider's plain-text output and emits one EvText per
// non-empty line. Aider's --no-pretty mode strips colour codes; --no-stream
// makes the reply land in one go (still split by \n in transit).
//
// Lines that look like aider's own banner / progress markers (start with
// "> " prompts, "Tokens:", "Cost:", git info) are filtered to avoid
// polluting the chat overlay.
func parseStdout(r io.Reader, s *session, log *obs.Logger) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if shouldFilterLine(line) {
			continue
		}
		s.emit(agent.Event{Type: agent.EvText, Text: line + "\n"})
	}
	if err := sc.Err(); err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("stream read: %v", err)})
	}
}

// shouldFilterLine drops aider's own progress / banner output that isn't
// part of the assistant reply. The list is conservative: when in doubt
// the line is forwarded so the user sees something rather than silence.
func shouldFilterLine(line string) bool {
	if line == "" {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	switch {
	case strings.HasPrefix(trimmed, "Aider v"),
		strings.HasPrefix(trimmed, "Model:"),
		strings.HasPrefix(trimmed, "Git repo:"),
		strings.HasPrefix(trimmed, "Repo-map:"),
		strings.HasPrefix(trimmed, "Use /help"),
		strings.HasPrefix(trimmed, "Tokens:"),
		strings.HasPrefix(trimmed, "Cost:"),
		strings.HasPrefix(trimmed, "> "),
		strings.HasPrefix(trimmed, "─"),
		strings.HasPrefix(trimmed, "━"):
		return true
	}
	return false
}

// ─── helpers ────────────────────────────────────────────────────────────

func drainStderr(r io.Reader, log *obs.Logger, sid agent.SessionID) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Warnf("aider stderr sid=%s: %s", sid, sc.Text())
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
