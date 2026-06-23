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
	found, err := r.Cancel("dev-1")
	if !found || err != nil {
		t.Fatalf("Cancel: want found=true err=nil, got found=%v err=%v", found, err)
	}
	if len(drv.interrupted) != 1 || drv.interrupted[0] != "sid-1" {
		t.Fatalf("want Interrupt(sid-1), got %v", drv.interrupted)
	}
	r.End("dev-1", tok)

	// ADR-028 §2.5.1 修订(撤回语义):打断 = 当没发生过,Cancel 只杀回合,不记
	// 任何待注入备注。再 Cancel 应是干净 no-op(found=false)。
	if found, _ := r.Cancel("dev-1"); found {
		t.Error("after End, Cancel must be a no-op (found=false)")
	}
}

func TestInflightCancelNoTurn(t *testing.T) {
	r := NewInflightRegistry()
	found, err := r.Cancel("dev-1")
	if found || err != nil {
		t.Fatalf("want found=false err=nil, got %v %v", found, err)
	}
}

func TestInflightCancelSingleEntryFallback(t *testing.T) {
	// Device omitted deviceId on the cancel path but the registry has exactly
	// one running turn (single-device deployment) → cancel that one.
	r := NewInflightRegistry()
	drv := &fakeInterruptDriver{name: "claude-code"}
	r.Begin("BBClaw-aabb", drv, "sid-9")
	found, err := r.Cancel("")
	if !found || err != nil {
		t.Fatalf("fallback: want found=true err=nil, got %v %v", found, err)
	}
	if len(drv.interrupted) != 1 {
		t.Fatalf("want 1 interrupt, got %d", len(drv.interrupted))
	}
}

func TestInflightCancelUnsupportedDriver(t *testing.T) {
	r := NewInflightRegistry()
	r.Begin("dev-1", &fakePlainDriver{name: "ollama"}, "sid-1")
	found, err := r.Cancel("dev-1")
	if !found {
		t.Fatal("want found=true")
	}
	if err == nil || !strings.Contains(err.Error(), "does not support interrupt") {
		t.Fatalf("want unsupported error, got %v", err)
	}
}

func TestInflightEndStaleTokenNoop(t *testing.T) {
	r := NewInflightRegistry()
	drv := &fakeInterruptDriver{name: "claude-code"}
	tok1 := r.Begin("dev-1", drv, "sid-1")
	tok2 := r.Begin("dev-1", drv, "sid-2") // retry overwrote the entry
	r.End("dev-1", tok1)                   // stale token must not clear sid-2
	found, _ := r.Cancel("dev-1")
	if !found {
		t.Fatal("stale End cleared the active turn")
	}
	r.End("dev-1", tok2)
	if found, _ := r.Cancel("dev-1"); found {
		t.Fatal("End with current token should clear the entry")
	}
}

func TestInflightInterruptErrorPropagates(t *testing.T) {
	r := NewInflightRegistry()
	wantErr := errors.New("boom")
	r.Begin("dev-1", &fakeInterruptDriver{name: "claude-code", err: wantErr}, "sid-1")
	found, err := r.Cancel("dev-1")
	if !found || !errors.Is(err, wantErr) {
		t.Fatalf("want found=true err=boom, got %v %v", found, err)
	}
}
