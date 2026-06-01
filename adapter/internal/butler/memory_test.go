package butler

import (
	"context"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// fakeMemory captures RecordTurn calls synchronously (the engine invokes it on
// the turn goroutine; the real impl enqueues non-blockingly).
type fakeMemory struct {
	calls []struct{ user, reply, cwd string }
}

func (m *fakeMemory) RecordTurn(userText, replyText, cwd string) {
	m.calls = append(m.calls, struct{ user, reply, cwd string }{userText, replyText, cwd})
}

// A butler session that ends cleanly must hand the turn to the MemoryWriter.
func TestRunTurn_MemoryRecordsButlerTurn(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	bsess, err := mgr.EnsureButler("dev-1", ButlerDriver, "/ws")
	if err != nil {
		t.Fatalf("EnsureButler: %v", err)
	}
	drv := newScriptedDriver(ButlerDriver, []agent.Event{{Type: agent.EvText, Text: "管家答复"}, {Type: agent.EvTurnEnd}})
	mem := &fakeMemory{}
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.Memory = mem
	eng := NewEngine(deps)

	_, err = eng.RunTurn(context.Background(), Request{
		Text:             "用户的话",
		RequestedDriver:  ButlerDriver,
		RequestedSession: string(bsess.ID),
		DeviceID:         "dev-1",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(mem.calls) != 1 {
		t.Fatalf("RecordTurn called %d times, want 1", len(mem.calls))
	}
	c := mem.calls[0]
	if c.user != "用户的话" || c.reply != "管家答复" || c.cwd != "/ws" {
		t.Fatalf("RecordTurn got %+v", c)
	}
}

// A non-butler logical session must NOT trigger memory recording.
func TestRunTurn_MemorySkipsNonButler(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	ls, err := mgr.CreateWithRole("dev-1", "mock", "/proj", "regular", logicalsession.RoleNone)
	if err != nil {
		t.Fatalf("CreateWithRole: %v", err)
	}
	drv := newScriptedDriver("mock", []agent.Event{{Type: agent.EvText, Text: "hi"}, {Type: agent.EvTurnEnd}})
	mem := &fakeMemory{}
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.Memory = mem
	eng := NewEngine(deps)

	_, err = eng.RunTurn(context.Background(), Request{
		Text:             "用户的话",
		RequestedDriver:  "mock",
		RequestedSession: string(ls.ID),
		DeviceID:         "dev-1",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(mem.calls) != 0 {
		t.Fatalf("RecordTurn called %d times for non-butler, want 0", len(mem.calls))
	}
}

// An error-only butler turn (errorCount>0) must NOT be recorded.
func TestRunTurn_MemorySkipsErrorTurn(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	bsess, err := mgr.EnsureButler("dev-1", ButlerDriver, "/ws")
	if err != nil {
		t.Fatalf("EnsureButler: %v", err)
	}
	// Error then turn_end, no text → turnEnded=true but errorCount=1.
	drv := newScriptedDriver(ButlerDriver, []agent.Event{{Type: agent.EvError, Text: "boom"}, {Type: agent.EvTurnEnd}})
	mem := &fakeMemory{}
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.Memory = mem
	eng := NewEngine(deps)

	_, err = eng.RunTurn(context.Background(), Request{
		Text:             "用户的话",
		RequestedDriver:  ButlerDriver,
		RequestedSession: string(bsess.ID),
		DeviceID:         "dev-1",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(mem.calls) != 0 {
		t.Fatalf("RecordTurn called %d times for error turn, want 0", len(mem.calls))
	}
}

// nil Memory dep must be a safe no-op (default path).
func TestRunTurn_MemoryNilIsNoop(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	bsess, err := mgr.EnsureButler("dev-1", ButlerDriver, "/ws")
	if err != nil {
		t.Fatalf("EnsureButler: %v", err)
	}
	drv := newScriptedDriver(ButlerDriver, []agent.Event{{Type: agent.EvText, Text: "hi"}, {Type: agent.EvTurnEnd}})
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.Memory = nil
	if _, err := NewEngine(deps).RunTurn(context.Background(), Request{
		Text:             "用户的话",
		RequestedDriver:  ButlerDriver,
		RequestedSession: string(bsess.ID),
		DeviceID:         "dev-1",
	}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
}
