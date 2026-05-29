package homeadapter

// ADR-019 server-driven menu: cloud-relay (home-adapter) side. Menu shaping is
// unit-tested in internal/agent/menu; these cover the envelope dispatch +
// action persistence specific to the cloud path.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func TestAgentProxyMenuDrivers(t *testing.T) {
	a := newProxyTestAdapter(t, newFakeAgentDriver("claude-code"))

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error { got = append(got, env); return nil }
	if err := a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m-1", Kind: "agent.menu",
		Payload: map[string]any{"id": "drivers"},
	}); err != nil {
		t.Fatalf("agent.menu: %v", err)
	}
	if len(got) != 1 || got[0].Type != "reply" || got[0].Kind != "agent.menu.reply" {
		t.Fatalf("reply shape: %+v", got)
	}
	if got[0].Payload["id"] != "drivers" {
		t.Fatalf("payload id wrong: %+v", got[0].Payload)
	}
	rows, ok := got[0].Payload["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows wrong: %+v", got[0].Payload["rows"])
	}
}

func TestAgentProxyMenuActionSetDriver(t *testing.T) {
	r := agent.NewRouter()
	r.Register(newFakeAgentDriver("claude-code"), obs.NewLogger())
	r.Register(newFakeAgentDriver("ollama"), obs.NewLogger())
	a := &Adapter{cfg: Config{HomeSiteID: "home-1"}, log: obs.NewLogger(), metrics: obs.NewMetrics()}
	a.SetRouter(r)
	st, err := driverstate.NewStore(filepath.Join(t.TempDir(), "ds.json"), obs.NewLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a.SetDriverState(st)

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error { got = append(got, env); return nil }
	if err := a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m-2", Kind: "agent.menu.action",
		Payload: map[string]any{"action": map[string]any{"type": "set_driver", "driver": "ollama"}},
	}); err != nil {
		t.Fatalf("agent.menu.action: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "agent.menu.action.reply" {
		t.Fatalf("reply shape: %+v", got)
	}
	if got[0].Payload["result"] != "closed" {
		t.Fatalf("result wrong: %+v", got[0].Payload)
	}
	if a.driverState.ActiveDriver() != "ollama" {
		t.Errorf("ActiveDriver=%q want ollama", a.driverState.ActiveDriver())
	}
}
