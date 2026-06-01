package butlermcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/claudecode"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// defaultMaxOutputBytes caps the worker result text the runner hands back to
// the butler. A worker turn can emit a long transcript; the butler only needs
// the final answer, and a multi-KB blob bloats the butler's own context window
// (ADR-021 §2). Beyond this size the middle is elided with a marker.
const defaultMaxOutputBytes = 8000

const truncationMarker = "\n…[output truncated]…\n"

// claudeDriver is the subset of agent.Driver the runner needs. claudecode.Driver
// satisfies it; tests inject a fake so Run can be exercised without spawning the
// real `claude` CLI.
type claudeDriver interface {
	Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error)
	Send(sid agent.SessionID, text string) error
	Events(sid agent.SessionID) <-chan agent.Event
	Stop(sid agent.SessionID) error
}

// ClaudeWorkerRunner implements WorkerRunner by spawning a claude-code worker
// session in the target cwd with `--permission-mode acceptEdits`, consuming its
// stream-json event stream, and returning the accumulated assistant text.
type ClaudeWorkerRunner struct {
	driver         claudeDriver
	maxOutputBytes int
}

// ClaudeRunnerOptions configure a ClaudeWorkerRunner.
type ClaudeRunnerOptions struct {
	// Bin is the path to the `claude` binary; empty resolves "claude" on PATH.
	Bin string
	// BaseURL / AuthToken map to ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN for
	// the worker subprocess. Empty values are not injected.
	BaseURL   string
	AuthToken string
	// ExtraArgs are appended after the fixed worker args (e.g. "--model", "…").
	// "--permission-mode acceptEdits" is always prepended by the runner so the
	// worker can edit files without an interactive approval round-trip.
	ExtraArgs []string
	// MaxOutputBytes caps the result text. <=0 uses defaultMaxOutputBytes.
	MaxOutputBytes int
	// Logger is the obs logger for the underlying claudecode driver. It MUST
	// write to stderr (obs.NewLoggerTo(os.Stderr)) — never stdout, which is the
	// MCP JSON-RPC channel. nil falls back to a stdout logger (tests only).
	Logger *obs.Logger
}

// NewClaudeWorkerRunner builds a production runner backed by the claudecode
// driver.
func NewClaudeWorkerRunner(opts ClaudeRunnerOptions) *ClaudeWorkerRunner {
	logger := opts.Logger
	if logger == nil {
		logger = obs.NewLogger()
	}
	// acceptEdits lets the worker apply file edits without blocking on an
	// approval prompt (ADR-021 §2: workers run unattended). Prepended so an
	// operator-supplied --permission-mode in ExtraArgs still wins (last-flag).
	extra := append([]string{"--permission-mode", "acceptEdits"}, opts.ExtraArgs...)
	var env map[string]string
	if opts.BaseURL != "" || opts.AuthToken != "" {
		env = make(map[string]string, 2)
		if opts.BaseURL != "" {
			env["ANTHROPIC_BASE_URL"] = opts.BaseURL
		}
		if opts.AuthToken != "" {
			env["ANTHROPIC_AUTH_TOKEN"] = opts.AuthToken
		}
	}
	d := claudecode.New(claudecode.Options{
		Bin:       opts.Bin,
		ExtraArgs: extra,
		Env:       env,
	}, logger)
	return newRunnerWithDriver(d, opts.MaxOutputBytes)
}

// newRunnerWithDriver is the testable constructor: it takes an already-built
// driver (real or fake) so unit tests can inject a mock.
func newRunnerWithDriver(d claudeDriver, maxOutputBytes int) *ClaudeWorkerRunner {
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	return &ClaudeWorkerRunner{driver: d, maxOutputBytes: maxOutputBytes}
}

// Run starts a worker session in cwd, sends task, and returns the worker's
// final text once the turn ends. Honours ctx cancellation.
func (r *ClaudeWorkerRunner) Run(ctx context.Context, cwd, task string) (string, error) {
	sid, err := r.driver.Start(ctx, agent.StartOpts{Cwd: cwd})
	if err != nil {
		return "", fmt.Errorf("butlermcp: start worker: %w", err)
	}
	// Always tear the session down so the driver doesn't leak it and any
	// in-flight subprocess is cancelled.
	defer func() { _ = r.driver.Stop(sid) }()

	events := r.driver.Events(sid)

	// Send blocks until the worker subprocess exits, so run it in a goroutine
	// while we drain events. The buffered channel keeps the goroutine from
	// leaking if we return early on ctx cancellation.
	sendErr := make(chan error, 1)
	go func() { sendErr <- r.driver.Send(sid, task) }()

	var sb strings.Builder
	var turnErr error
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// Channel closed before EvTurnEnd — surface whatever Send
				// reported, else a generic error.
				if turnErr != nil {
					return "", turnErr
				}
				if se := drainSendErr(sendErr); se != nil {
					return "", se
				}
				return clampOutput(sb.String(), r.maxOutputBytes), nil
			}
			switch ev.Type {
			case agent.EvText:
				sb.WriteString(ev.Text)
			case agent.EvError:
				if turnErr == nil && strings.TrimSpace(ev.Text) != "" {
					turnErr = errors.New(ev.Text)
				}
			case agent.EvTurnEnd:
				if turnErr != nil {
					return "", turnErr
				}
				return clampOutput(sb.String(), r.maxOutputBytes), nil
			}
		}
	}
}

// drainSendErr does a non-blocking read of the Send result.
func drainSendErr(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	default:
		return nil
	}
}

// clampOutput truncates s to at most max bytes, eliding the middle (the head
// carries context, the tail carries the final answer) with a marker.
func clampOutput(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= len(truncationMarker) {
		return s[:max]
	}
	keep := max - len(truncationMarker)
	head := keep / 2
	tail := keep - head
	return s[:head] + truncationMarker + s[len(s)-tail:]
}
