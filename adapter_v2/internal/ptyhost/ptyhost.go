// Package ptyhost spawns an interactive CLI (claude / codex / opencode / ...)
// inside a pseudo-terminal and exposes its raw byte streams. It is deliberately
// CLI-agnostic: it knows nothing about the agent's output format. This is what
// lets v2 replace v1's per-CLI driver matrix with a single transport.
//
// Since M2 (agent-runner integration) the actual PTY placement is delegated to
// the agent-runner host executor (github.com/zhoushoujianwork/agent-runner):
// same creack/pty underneath, plus the SDK's lifecycle hardening — graceful
// SIGTERM → grace → SIGKILL teardown instead of a straight Kill, CLAUDECODE
// env stripping, EIO→EOF mapping on the master read after child exit, and
// optional ExtraDirs context mounting. This package keeps only the v2-specific
// bits: the CLI-agnostic Config surface and StartupInput playback.
package ptyhost

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/zhoushoujianwork/agent-runner/executor/host"
	"github.com/zhoushoujianwork/agent-runner/runner"
)

// ErrEmptyArgv is returned when Config.Argv has no program to run.
var ErrEmptyArgv = errors.New("ptyhost: empty argv")

const (
	defaultCols = 80
	defaultRows = 24
)

// executor is the shared agent-runner host executor. Its default termination
// grace (2s between SIGTERM and SIGKILL) is what Close relies on for a clean
// child shutdown; sessions that need a longer flush window can rely on the
// child's own signal handling within that budget.
var executor = host.New()

// Size is the terminal grid size handed to the PTY and the VT screen.
type Size struct {
	Cols uint16
	Rows uint16
}

func (s Size) orDefault() Size {
	if s.Cols == 0 {
		s.Cols = defaultCols
	}
	if s.Rows == 0 {
		s.Rows = defaultRows
	}
	return s
}

// Config describes how to launch the agent CLI under a PTY.
type Config struct {
	// Argv[0] is the program (e.g. "claude"); the rest are args. NOTE: v2
	// runs the interactive TUI — do NOT pass "-p"/"--output-format" here.
	Argv []string
	// Cwd is the working directory for the spawned process (project root).
	Cwd string
	// Env are extra environment variables merged onto the parent environment.
	// TERM=xterm-256color is set by the host unless the caller overrides it.
	Env map[string]string
	// InitialSize is the starting grid; clients resize later via PTY.Resize.
	InitialSize Size
	// StartupInput is written into the PTY shortly after spawn, one chunk at a
	// time with each chunk's inter-write Delay. It exists to auto-dismiss a CLI's
	// first-run onboarding prompts that would otherwise block an interactive
	// viewer — e.g. claude's "Try the new fullscreen renderer?" upsell: a couple
	// of staggered Enters pick the highlighted default and move on. The playback
	// runs in its own goroutine, so Spawn still returns immediately. Empty ⇒ no
	// injection. Write errors are ignored (the child may have already exited).
	StartupInput []StartupChunk
	// ExtraDirs are agent-runner context roots linked into Cwd for the process
	// lifetime (project .claude/.agent skills, agents, commands). See the
	// agent-runner ExtraDir docs for discovery/exact-mode semantics.
	ExtraDirs []runner.ExtraDir
}

// StartupChunk is one delayed write replayed into a freshly spawned PTY. Delay
// is measured from the previous chunk (or from spawn, for the first), so a slice
// plays back as a simple cadence without absolute clocks.
type StartupChunk struct {
	Delay time.Duration
	Data  []byte
}

// PTY is a live pseudo-terminal wrapping one CLI process. Read returns the
// process's raw stdout/stderr byte stream (ANSI escapes included); Write feeds
// its stdin (keystrokes / injected ASR text). It is safe for one reader and
// one writer goroutine.
type PTY interface {
	io.ReadWriteCloser
	// Resize updates the terminal grid (forwarded to the kernel PTY).
	Resize(Size) error
	// Wait blocks until the child exits and returns its exit code.
	Wait() (int, error)
}

// ptySession adapts an agent-runner PTYProcess to the v2 PTY surface.
type ptySession struct {
	proc runner.PTYProcess
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.proc.Output().Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.proc.Input().Write(b) }

// Close tears down the child through agent-runner's graceful chain (SIGTERM →
// grace → SIGKILL). The master fd is released by the executor's reaper once the
// child is gone; callers that never call Wait leak nothing.
func (p *ptySession) Close() error {
	return p.proc.Cancel()
}

func (p *ptySession) Resize(s Size) error {
	s = s.orDefault()
	return p.proc.Resize(runner.TermSize{Cols: s.Cols, Rows: s.Rows})
}

// Wait reaps the child and returns its exit code. A non-zero exit is reported
// via the code, not as an error; err is non-nil only for genuine wait failures.
func (p *ptySession) Wait() (int, error) {
	status, err := p.proc.Wait()
	if err != nil {
		return -1, err
	}
	return status.ExitCode, nil
}

// Spawn launches cfg.Argv under a fresh PTY at the requested initial size.
func Spawn(cfg Config) (PTY, error) {
	if len(cfg.Argv) == 0 {
		return nil, ErrEmptyArgv
	}
	size := cfg.InitialSize.orDefault()
	spec := runner.CommandSpec{
		Argv:      cfg.Argv,
		Dir:       cfg.Cwd,
		Env:       cfg.Env,
		ExtraDirs: cfg.ExtraDirs,
	}
	// Lifecycle is owned by Close/Wait (session.Manager kill/GC), matching the
	// previous behaviour where no context ever cancelled a spawned PTY.
	proc, err := executor.StartPTY(context.Background(), spec, runner.TermSize{Cols: size.Cols, Rows: size.Rows})
	if err != nil {
		return nil, err
	}
	s := &ptySession{proc: proc}
	if len(cfg.StartupInput) > 0 {
		go playStartupInput(s, cfg.StartupInput)
	}
	return s, nil
}

// playStartupInput replays cfg.StartupInput into a freshly spawned PTY: it sleeps
// each chunk's Delay, then writes its Data. Errors are intentionally ignored —
// if the child has exited or closed the PTY there is nothing left to dismiss.
// Runs in its own goroutine so Spawn returns without blocking on the cadence.
func playStartupInput(p PTY, chunks []StartupChunk) {
	for _, c := range chunks {
		if c.Delay > 0 {
			time.Sleep(c.Delay)
		}
		if len(c.Data) > 0 {
			_, _ = p.Write(c.Data)
		}
	}
}
