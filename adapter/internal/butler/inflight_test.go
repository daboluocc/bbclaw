package butler

import (
	"errors"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// fakeInterruptDriver implements agent.Driver + agent.Interrupter, recording
// Interrupt calls.
type fakeInterruptDriver struct {
	agent.Driver
	name        string
	interrupted []agent.SessionID
	err         error
}

func (f *fakeInterruptDriver) Name() string { return f.name }
func (f *fakeInterruptDriver) Interrupt(sid agent.SessionID) error {
	f.interrupted = append(f.interrupted, sid)
	return f.err
}

// fakePlainDriver implements agent.Driver WITHOUT Interrupter.
type fakePlainDriver struct {
	agent.Driver
	name string
}

func (f *fakePlainDriver) Name() string { return f.name }

func TestInflightCancelInterruptsCurrentTurn(t *testing.T) {
	r := NewInflightRegistry()
	drv := &fakeInterruptDriver{name: "claude-code"}

	tok := r.Begin("dev-1", drv, "sid-1")
	found, err := r.Cancel("dev-1", "今天天气晴。")
	if !found || err != nil {
		t.Fatalf("Cancel: want found=true err=nil, got found=%v err=%v", found, err)
	}
	if len(drv.interrupted) != 1 || drv.interrupted[0] != "sid-1" {
		t.Fatalf("want Interrupt(sid-1), got %v", drv.interrupted)
	}
	r.End("dev-1", tok)

	// Note was recorded and renders with the played text.
	note := r.ConsumePromptNote("dev-1")
	if !strings.Contains(note, "今天天气晴。") || !strings.Contains(note, "打断") {
		t.Errorf("note should mention played text + interruption, got %q", note)
	}
	// Consumed — second read is empty.
	if n2 := r.ConsumePromptNote("dev-1"); n2 != "" {
		t.Errorf("note must be consumed once, got %q", n2)
	}
}

func TestInflightCancelNoTurn(t *testing.T) {
	r := NewInflightRegistry()
	found, err := r.Cancel("dev-1", "")
	if found || err != nil {
		t.Fatalf("want found=false err=nil, got %v %v", found, err)
	}
	// NoteInterruption still lets the next turn know playback was cut.
	r.NoteInterruption("dev-1", "")
	if note := r.ConsumePromptNote("dev-1"); note == "" {
		t.Error("want generic interruption note, got empty")
	}
}

func TestInflightCancelSingleEntryFallback(t *testing.T) {
	// Device omitted deviceId on the cancel path but the registry has exactly
	// one running turn (single-device deployment) → cancel that one.
	r := NewInflightRegistry()
	drv := &fakeInterruptDriver{name: "claude-code"}
	r.Begin("BBClaw-aabb", drv, "sid-9")
	found, err := r.Cancel("", "句子。")
	if !found || err != nil {
		t.Fatalf("fallback: want found=true err=nil, got %v %v", found, err)
	}
	if len(drv.interrupted) != 1 {
		t.Fatalf("want 1 interrupt, got %d", len(drv.interrupted))
	}
	// The note landed under the real device id and the same single-entry
	// fallback applies on consumption.
	if note := r.ConsumePromptNote(""); note == "" {
		t.Error("want note via single-entry fallback, got empty")
	}
}

func TestInflightCancelUnsupportedDriver(t *testing.T) {
	r := NewInflightRegistry()
	r.Begin("dev-1", &fakePlainDriver{name: "ollama"}, "sid-1")
	found, err := r.Cancel("dev-1", "")
	if !found {
		t.Fatal("want found=true")
	}
	if err == nil || !strings.Contains(err.Error(), "does not support interrupt") {
		t.Fatalf("want unsupported error, got %v", err)
	}
	// Even so the interruption note is recorded.
	if note := r.ConsumePromptNote("dev-1"); note == "" {
		t.Error("want note even when driver can't interrupt")
	}
}

func TestInflightEndStaleTokenNoop(t *testing.T) {
	r := NewInflightRegistry()
	drv := &fakeInterruptDriver{name: "claude-code"}
	tok1 := r.Begin("dev-1", drv, "sid-1")
	tok2 := r.Begin("dev-1", drv, "sid-2") // retry overwrote the entry
	r.End("dev-1", tok1)                   // stale token must not clear sid-2
	found, _ := r.Cancel("dev-1", "")
	if !found {
		t.Fatal("stale End cleared the active turn")
	}
	r.End("dev-1", tok2)
	if found, _ := r.Cancel("dev-1", ""); found {
		t.Fatal("End with current token should clear the entry")
	}
}

func TestInflightInterruptErrorPropagates(t *testing.T) {
	r := NewInflightRegistry()
	wantErr := errors.New("boom")
	r.Begin("dev-1", &fakeInterruptDriver{name: "claude-code", err: wantErr}, "sid-1")
	found, err := r.Cancel("dev-1", "")
	if !found || !errors.Is(err, wantErr) {
		t.Fatalf("want found=true err=boom, got %v %v", found, err)
	}
}
