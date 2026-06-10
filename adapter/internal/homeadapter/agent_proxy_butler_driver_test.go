package homeadapter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/driverstate"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// butlerCapable returns a fake driver advertising Capabilities.Butler=true.
func butlerCapable(name string) *fakeAgentDriver {
	d := newFakeAgentDriver(name)
	d.caps = agent.Capabilities{Streaming: true, MaxInputBytes: 4096, Butler: true}
	return d
}

func newButlerDriverTestAdapter(t *testing.T, drivers ...agent.Driver) *Adapter {
	t.Helper()
	a := newProxyTestAdapter(t, nil)
	for _, d := range drivers {
		a.router.Register(d, obs.NewLogger())
	}
	st, err := driverstate.NewStore(filepath.Join(t.TempDir(), "driver_state.json"), obs.NewLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a.SetDriverState(st)
	return a
}

// TestAgentProxyButlerDriverSet mirrors the LAN PUT /v1/agent/butler_driver:
// the cloud relay must accept butler-capable drivers, reject the rest, and not
// touch active_driver (ADR-024 §1 cloud parity).
func TestAgentProxyButlerFollowsActiveDriver(t *testing.T) {
	a := newButlerDriverTestAdapter(t, butlerCapable("claude-code"), butlerCapable("opencode"), newFakeAgentDriver("ollama"))

	if got := a.resolveButlerDriver(); got != "claude-code" {
		t.Errorf("default butler want claude-code, got %q", got)
	}
	if err := a.driverState.SetActiveDriver("opencode"); err != nil {
		t.Fatalf("SetActiveDriver: %v", err)
	}
	a.router.SetDefault("opencode")
	if got := a.resolveButlerDriver(); got != "opencode" {
		t.Errorf("active=opencode: butler want opencode, got %q", got)
	}
	// Non-butler-capable active → fallback claude-code.
	if err := a.driverState.SetActiveDriver("ollama"); err != nil {
		t.Fatalf("SetActiveDriver: %v", err)
	}
	a.router.SetDefault("ollama")
	if got := a.resolveButlerDriver(); got != "claude-code" {
		t.Errorf("active=ollama: butler want claude-code fallback, got %q", got)
	}
}

// TestAgentProxyDriversReplyButlerFields asserts the cloud drivers reply
// carries butler_driver + per-row butler_capable, matching the LAN response.
func TestAgentProxyDriversReplyButlerFields(t *testing.T) {
	a := newButlerDriverTestAdapter(t, butlerCapable("claude-code"), newFakeAgentDriver("ollama"))

	var got []CloudEnvelope
	write := func(env CloudEnvelope) error { got = append(got, env); return nil }
	if err := a.handleRequest(context.Background(), write, CloudEnvelope{
		Type: "request", MessageID: "m", Kind: "agent.drivers",
	}); err != nil {
		t.Fatalf("agent.drivers: err=%v", err)
	}
	env := got[0]
	if env.Payload["butler_driver"] != "claude-code" {
		t.Errorf("want butler_driver=claude-code fallback, got %v", env.Payload["butler_driver"])
	}
	rows, _ := env.Payload["drivers"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	for _, row := range rows {
		switch row["name"] {
		case "claude-code":
			if row["butler_capable"] != true {
				t.Errorf("claude-code butler_capable should be true, got %v", row["butler_capable"])
			}
		case "ollama":
			if row["butler_capable"] != false {
				t.Errorf("ollama butler_capable should be false, got %v", row["butler_capable"])
			}
		}
	}
}
