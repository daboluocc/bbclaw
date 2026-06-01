package butlermcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// fakeDriver is an injectable claudeDriver. Send replays a scripted event
// sequence onto the session channel, then returns sendErr. It mirrors the real
// driver's invariant: emits are guarded by a stop signal so Stop can never race
// a write onto a closed channel.
type fakeDriver struct {
	events  []agent.Event
	sendErr error

	startErr error
	startCwd string // captured Cwd from StartOpts

	gate           chan struct{} // if non-nil, Send waits for it before emitting
	closeAfterSend bool          // if true, Send closes ch after emitting (no concurrent writer)

	ch   chan agent.Event
	stop chan struct{}

	stopped bool
}

func (f *fakeDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	f.startCwd = opts.Cwd
	f.ch = make(chan agent.Event, 64)
	f.stop = make(chan struct{})
	return agent.SessionID("fake-sid"), nil
}

func (f *fakeDriver) Send(sid agent.SessionID, text string) error {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-f.stop:
			return f.sendErr
		}
	}
	for _, ev := range f.events {
		select {
		case f.ch <- ev:
		case <-f.stop:
			return f.sendErr
		}
	}
	if f.closeAfterSend {
		close(f.ch) // Send is the sole writer here — safe to close.
	}
	return f.sendErr
}

func (f *fakeDriver) Events(sid agent.SessionID) <-chan agent.Event { return f.ch }

func (f *fakeDriver) Stop(sid agent.SessionID) error {
	f.stopped = true
	if f.stop != nil {
		select {
		case <-f.stop: // already closed
		default:
			close(f.stop)
		}
	}
	return nil
}

func TestRunnerCollectsText(t *testing.T) {
	d := &fakeDriver{events: []agent.Event{
		{Type: agent.EvSessionInit, Text: "cli-sid"},
		{Type: agent.EvText, Text: "Hello "},
		{Type: agent.EvText, Text: "world"},
		{Type: agent.EvTokens},
		{Type: agent.EvTurnEnd},
	}}
	r := newRunnerWithDriver(d, 0)
	out, err := r.Run(context.Background(), "/p/proj", "do x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "Hello world" {
		t.Fatalf("out=%q", out)
	}
	if d.startCwd != "/p/proj" {
		t.Errorf("cwd not propagated: %q", d.startCwd)
	}
	if !d.stopped {
		t.Error("session not stopped")
	}
}

func TestRunnerSurfacesError(t *testing.T) {
	d := &fakeDriver{events: []agent.Event{
		{Type: agent.EvText, Text: "partial"},
		{Type: agent.EvError, Text: "claude-code exit: 1"},
		{Type: agent.EvTurnEnd},
	}}
	r := newRunnerWithDriver(d, 0)
	_, err := r.Run(context.Background(), "/p/proj", "do x")
	if err == nil || !strings.Contains(err.Error(), "claude-code exit: 1") {
		t.Fatalf("want EvError surfaced, got %v", err)
	}
}

func TestRunnerCtxCancel(t *testing.T) {
	gate := make(chan struct{}) // Send blocks forever
	d := &fakeDriver{gate: gate, events: []agent.Event{{Type: agent.EvTurnEnd}}}
	r := newRunnerWithDriver(d, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := r.Run(ctx, "/p/proj", "long task")
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	close(gate)
}

func TestRunnerStartError(t *testing.T) {
	d := &fakeDriver{startErr: context.DeadlineExceeded}
	r := newRunnerWithDriver(d, 0)
	if _, err := r.Run(context.Background(), "/p/proj", "x"); err == nil {
		t.Fatal("expected start error")
	}
}

func TestRunnerChannelClosedWithoutTurnEnd(t *testing.T) {
	// No EvTurnEnd: the driver closes the event channel after emitting. The
	// runner must return the accumulated text rather than hang or panic.
	d := &fakeDriver{
		events:         []agent.Event{{Type: agent.EvText, Text: "abc"}},
		closeAfterSend: true,
	}
	r := newRunnerWithDriver(d, 0)
	out, err := r.Run(context.Background(), "/p/proj", "x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "abc" {
		t.Fatalf("out=%q want abc", out)
	}
}

func TestClampOutput(t *testing.T) {
	if got := clampOutput("short", 100); got != "short" {
		t.Errorf("no-trim: %q", got)
	}
	long := strings.Repeat("x", 100) + strings.Repeat("y", 100)
	got := clampOutput(long, 60)
	if len(got) > 60 {
		t.Errorf("clamped len=%d > 60", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("missing truncation marker: %q", got)
	}
	if !strings.HasPrefix(got, "x") || !strings.HasSuffix(got, "y") {
		t.Errorf("head/tail not preserved: %q", got)
	}
}
