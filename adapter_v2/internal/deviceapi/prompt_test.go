package deviceapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/extract"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// promptMockCLI prints a claude-style blocking permission menu and then idles,
// consuming stdin (so an injected answer doesn't block). The menu carries the
// "❯" highlight pointer (\xe2\x9d\xaf = U+276F), two options, and the
// "Esc to cancel" footer — everything ParsePrompt anchors on.
const promptMockCLI = "" +
	"printf '\\033[2J\\033[H'\n" +
	"printf 'Do you want to proceed?\\r\\n'\n" +
	"printf '\\xe2\\x9d\\xaf 1. Yes\\r\\n'\n" +
	"printf '  2. No\\r\\n'\n" +
	"printf 'Esc to cancel\\r\\n'\n" +
	"cat >/dev/null\n"

func newPromptMockSession(t *testing.T) *session.Session {
	t.Helper()
	m := session.NewManager()
	s, err := m.Create("devprompt", ptyhost.Config{
		Argv:        []string{"bash", "-c", promptMockCLI},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create prompt mock session: %v", err)
	}
	return s
}

// recPrompt implements Events AND PromptObserver, recording every prompt event.
type recPrompt struct {
	mu     sync.Mutex
	opens  []PromptSpec
	closes []closeRec
}
type closeRec struct{ id, reason string }

func (r *recPrompt) ReplyDelta(string)    {}
func (r *recPrompt) ReplyComplete(string) {}
func (r *recPrompt) ToolStep(_, _ string) {}
func (r *recPrompt) TurnIdle()            {}
func (r *recPrompt) PromptOpen(p PromptSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens = append(r.opens, p)
}
func (r *recPrompt) PromptClosed(id, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes = append(r.closes, closeRec{id, reason})
}
func (r *recPrompt) openCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opens)
}
func (r *recPrompt) waitOpen(t *testing.T, d time.Duration) PromptSpec {
	t.Helper()
	deadline := time.After(d)
	for {
		r.mu.Lock()
		if len(r.opens) > 0 {
			p := r.opens[0]
			r.mu.Unlock()
			return p
		}
		r.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("PromptOpen never fired")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func (r *recPrompt) waitClose(t *testing.T, id, reason string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		r.mu.Lock()
		for _, c := range r.closes {
			if c.id == id && c.reason == reason {
				r.mu.Unlock()
				return
			}
		}
		r.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("PromptClosed{%s,%s} never fired", id, reason)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func (r *recPrompt) closeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.closes)
}

// TestConfirmPromptForwardAndSelect: a permission menu is detected, forwarded
// with the right spec, answered via SelectPromptOption, and a stale re-select is
// a safe no-op.
func TestConfirmPromptForwardAndSelect(t *testing.T) {
	s := newPromptMockSession(t)
	obs := &recPrompt{}
	b := New(s, nil, nil, nil, Config{
		Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond,
		ConfirmPrompts: true, PromptTimeout: 30 * time.Second,
	})
	b.SetEvents(obs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	spec := obs.waitOpen(t, 3*time.Second)
	if spec.Kind != "permission" {
		t.Errorf("Kind = %q, want permission", spec.Kind)
	}
	if spec.Mechanism != "digit" {
		t.Errorf("Mechanism = %q, want digit", spec.Mechanism)
	}
	if len(spec.Options) != 2 || spec.Options[0].Key != "1" || spec.Options[1].Key != "2" {
		t.Fatalf("Options = %+v, want keys 1,2", spec.Options)
	}
	if !spec.Options[0].Default {
		t.Error("option 1 (❯) should be Default")
	}

	if err := b.SelectPromptOption(spec.ID, "1"); err != nil {
		t.Fatalf("SelectPromptOption: %v", err)
	}
	obs.waitClose(t, spec.ID, "answered", 2*time.Second)

	// A stale re-select against the answered id is an ack-and-drop no-op: no error,
	// no second close.
	before := obs.closeCount()
	if err := b.SelectPromptOption(spec.ID, "1"); err != nil {
		t.Fatalf("stale SelectPromptOption should be a no-op, got %v", err)
	}
	if obs.closeCount() != before {
		t.Error("stale select emitted an extra PromptClosed")
	}
}

// TestConfirmPromptTimeoutAutoDeny: an unanswered menu auto-resolves the safe way
// (PromptClosed{timeout}) — never auto-approves (§11 invariant).
func TestConfirmPromptTimeoutAutoDeny(t *testing.T) {
	s := newPromptMockSession(t)
	obs := &recPrompt{}
	b := New(s, nil, nil, nil, Config{
		Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond,
		ConfirmPrompts: true, PromptTimeout: 300 * time.Millisecond,
	})
	b.SetEvents(obs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	spec := obs.waitOpen(t, 3*time.Second)
	obs.waitClose(t, spec.ID, "timeout", 3*time.Second)
}

// TestConfirmPromptOffNoForward: with ConfirmPrompts unset (the default), a menu
// on screen is NOT forwarded — exact pre-ADR-033 behaviour.
func TestConfirmPromptOffNoForward(t *testing.T) {
	s := newPromptMockSession(t)
	obs := &recPrompt{}
	b := New(s, nil, nil, nil, Config{Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond})
	b.SetEvents(obs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	time.Sleep(700 * time.Millisecond)
	if n := obs.openCount(); n != 0 {
		t.Errorf("forwarded %d prompts while ConfirmPrompts is off, want 0", n)
	}
}

// TestConfirmPromptNoObserverNoPanic: a nil PromptObserver (transport doesn't
// implement it) still auto-denies on timeout without panicking.
func TestConfirmPromptNoObserverNoPanic(t *testing.T) {
	s := newPromptMockSession(t)
	b := New(s, nil, nil, nil, Config{
		Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond,
		ConfirmPrompts: true, PromptTimeout: 200 * time.Millisecond,
	})
	// no SetEvents → promptObs is nil

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = b.Run(ctx); close(done) }()
	time.Sleep(800 * time.Millisecond) // long enough to open + time out + auto-deny
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit")
	}
}

// TestSelectPromptOptionUnknownKey: an option key not on the menu is dropped
// safely (no mis-inject), even for the live promptId.
func TestSelectPromptOptionUnknownKey(t *testing.T) {
	s := newPromptMockSession(t)
	obs := &recPrompt{}
	b := New(s, nil, nil, nil, Config{
		Cols: 80, Rows: 24, PollInterval: 40 * time.Millisecond,
		ConfirmPrompts: true, PromptTimeout: 30 * time.Second,
	})
	b.SetEvents(obs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	spec := obs.waitOpen(t, 3*time.Second)
	if err := b.SelectPromptOption(spec.ID, "9"); err != nil {
		t.Fatalf("unknown-key select should no-op, got %v", err)
	}
	if obs.closeCount() != 0 {
		t.Error("unknown-key select should not close the prompt")
	}
}

// TestDenyKeyFor: the safe auto-deny picks the "No" option's key, or "" (→ ESC)
// when there is no explicit deny option.
func TestDenyKeyFor(t *testing.T) {
	mk := func(labels, keys []string) extract.Prompt {
		var opts []extract.PromptOption
		for i := range labels {
			opts = append(opts, extract.PromptOption{Key: keys[i], Label: labels[i]})
		}
		return extract.Prompt{Options: opts}
	}
	cases := []struct {
		name   string
		labels []string
		keys   []string
		want   string
	}{
		{"yes/no", []string{"Yes", "No"}, []string{"1", "2"}, "2"},
		{"three", []string{"Yes", "Yes, always", "No, and tell Claude"}, []string{"1", "2", "3"}, "3"},
		{"no-deny-option", []string{"Proceed", "Abort"}, []string{"1", "2"}, ""},
	}
	for _, c := range cases {
		if got := denyKeyFor(mk(c.labels, c.keys)); got != c.want {
			t.Errorf("%s: denyKeyFor = %q, want %q", c.name, got, c.want)
		}
	}
}
