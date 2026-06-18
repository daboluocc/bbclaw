// Package ptyhost spawns an interactive CLI (claude / codex / opencode / ...)
// inside a pseudo-terminal and exposes its raw byte streams. It is deliberately
// CLI-agnostic: it knows nothing about the agent's output format. This is what
// lets v2 replace v1's per-CLI driver matrix with a single transport.
//
// Compare: dinotty src/pty.rs (the reference implementation we modelled on).
package ptyhost

import (
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// ErrEmptyArgv is returned when Config.Argv has no program to run.
var ErrEmptyArgv = errors.New("ptyhost: empty argv")

const (
	defaultCols = 80
	defaultRows = 24
)

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

// ptySession is the concrete PTY backed by creack/pty.
type ptySession struct {
	f   *os.File  // PTY master: read = child stdout/stderr, write = child stdin
	cmd *exec.Cmd // the spawned child
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.f.Write(b) }

// Close tears down the PTY and best-effort kills the child. Reaping happens in
// Wait; callers that never call Wait still release the master fd here.
func (p *ptySession) Close() error {
	err := p.f.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return err
}

func (p *ptySession) Resize(s Size) error {
	s = s.orDefault()
	return pty.Setsize(p.f, &pty.Winsize{Rows: s.Rows, Cols: s.Cols})
}

// Wait reaps the child and returns its exit code. A non-zero exit (ExitError)
// is reported via the code, not as an error; err is non-nil only for genuine
// wait failures.
func (p *ptySession) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Spawn launches cfg.Argv under a fresh PTY at the requested initial size.
func Spawn(cfg Config) (PTY, error) {
	if len(cfg.Argv) == 0 {
		return nil, ErrEmptyArgv
	}

	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	cmd.Env = buildEnv(cfg.Env)

	size := cfg.InitialSize.orDefault()
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: size.Rows, Cols: size.Cols})
	if err != nil {
		return nil, err
	}
	return &ptySession{f: f, cmd: cmd}, nil
}

// buildEnv merges extra vars onto the parent environment and ensures TERM is
// set so the CLI emits a full xterm-256color TUI.
func buildEnv(extra map[string]string) []string {
	env := os.Environ()
	hasTerm := false
	for k, v := range extra {
		env = append(env, k+"="+v)
		if k == "TERM" {
			hasTerm = true
		}
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}
