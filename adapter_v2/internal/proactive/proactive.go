// Package proactive runs an Agent turn WITHOUT a user PTT and returns the reply
// text, so the reminder scheduler can "run a task and report the result" (ADR-042
// §3.3, Task #10) — e.g. "check the flash log for errors and tell me".
//
// Why a dedicated runner (not the device bridge): the cloud reply path is
// request-scoped (cloudrelay.cloudEvents only forwards a reply while a device
// request is in flight), so a proactively-injected turn's reply never reaches the
// device that way. Running and delivering are therefore decoupled: the Runner
// executes the turn HEADLESS on an isolated worker session and hands the reply
// text back; the caller delivers it via the notification channel (§3.1).
//
// The Runner reuses deviceapi.Bridge's turn-boundary detection + reply extraction
// (the validated PTY screen-scrape) with a capturing Events observer and nil
// audio (no ASR/TTS/sink). One turn at a time (serialized), so a slow task can't
// overlap another.
package proactive

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// ErrBusy is returned when a RunOnce is already in progress (one turn at a time).
var ErrBusy = errors.New("proactive: a turn is already running")

// defaultTimeout bounds a single proactive turn (ADR-042 §5: a background task
// must not run unbounded when the user isn't watching).
const defaultTimeout = 4 * time.Minute

// Runner owns one isolated worker session + a headless Bridge onto it, kept warm
// for the process lifetime. RunOnce injects a prompt and returns the reply.
type Runner struct {
	bridge *deviceapi.Bridge
	cap    *capture

	mu      sync.Mutex // serializes RunOnce (one worker turn at a time)
	running bool
}

// New builds a Runner: it creates the worker session under id via mgr, wires a
// headless Bridge (nil ASR/TTS/sink) with Warmup so claude's startup is driven
// past before the first turn, and starts the Bridge's Run loop under ctx. cfg is
// the worker's spawn config (butler.WorkerConfig). cols/rows must match the
// worker session's grid so extraction sees the same wrapping.
func New(ctx context.Context, mgr *session.Manager, id string, cfg ptyhost.Config, cols, rows int) (*Runner, error) {
	sess, err := mgr.GetOrCreate(id, cfg)
	if err != nil {
		return nil, err
	}
	// Warmup: drive claude past its trust/startup prompt before turn 1.
	return newRunner(ctx, sess, cols, rows, true), nil
}

// newRunner wires a headless Bridge (capturing Events, nil audio) onto sess and
// starts its Run loop. warmup is a param so tests can drive a mock CLI that has
// no claude startup handshake.
func newRunner(ctx context.Context, sess *session.Session, cols, rows int, warmup bool) *Runner {
	cap := &capture{}
	b := deviceapi.New(sess, nil, nil, nil, deviceapi.Config{
		Cols:   cols,
		Rows:   rows,
		Warmup: warmup,
	})
	b.SetEvents(cap)
	go b.Run(ctx)
	return &Runner{bridge: b, cap: cap}
}

// RunOnce injects prompt as one headless turn and returns the assistant's reply
// text (empty for a tool-only / blank turn). timeout<=0 uses defaultTimeout.
// Only one turn runs at a time; a concurrent call gets ErrBusy.
func (r *Runner) RunOnce(ctx context.Context, prompt string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return "", ErrBusy
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	reply, idle := r.cap.begin()
	defer r.cap.end()

	if err := r.bridge.SubmitVoiceTurn(prompt); err != nil {
		return "", err
	}

	select {
	case text := <-reply:
		return text, nil
	case <-idle:
		// Turn ended without a speakable reply. ReplyComplete fires just before
		// TurnIdle, so a real reply would have been taken above; a race where both
		// are ready resolves to the reply on the next check.
		select {
		case text := <-reply:
			return text, nil
		default:
			return "", nil
		}
	case <-time.After(timeout):
		// Abort the stuck turn so the worker is clean for the next task.
		_ = r.bridge.Interrupt()
		return "", context.DeadlineExceeded
	case <-ctx.Done():
		_ = r.bridge.Interrupt()
		return "", ctx.Err()
	}
}

// capture implements deviceapi.Events, routing each turn's final reply text (and
// the turn-idle signal) to the channels RunOnce is waiting on. Between turns the
// channels are nil so stray events are dropped. Guarded by mu since Bridge.Run
// calls these from its own goroutine while RunOnce swaps the channels.
type capture struct {
	mu   sync.Mutex
	repl chan string
	idle chan struct{}
}

func (c *capture) begin() (chan string, chan struct{}) {
	repl := make(chan string, 1)
	idle := make(chan struct{}, 1)
	c.mu.Lock()
	c.repl, c.idle = repl, idle
	c.mu.Unlock()
	return repl, idle
}

func (c *capture) end() {
	c.mu.Lock()
	c.repl, c.idle = nil, nil
	c.mu.Unlock()
}

func (c *capture) ReplyComplete(text string) {
	c.mu.Lock()
	ch := c.repl
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- text:
		default:
		}
	}
}

func (c *capture) TurnIdle() {
	c.mu.Lock()
	ch := c.idle
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Unused Events methods (headless runner shows no live UI).
func (c *capture) ReplyDelta(string)       {}
func (c *capture) ToolStep(string, string) {}
