package butler

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

func newTestManager(t *testing.T, defaultCwd string) *logicalsession.Manager {
	t.Helper()
	mgr, err := logicalsession.NewManager(filepath.Join(t.TempDir(), "sessions.json"), defaultCwd, obs.NewLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func okScript() [][]agent.Event {
	return [][]agent.Event{{{Type: agent.EvText, Text: "hi"}, {Type: agent.EvTurnEnd}}}
}

// A butler logical session (Role=butler) must get StartOpts.MCPConfig wired and
// run in its workspace cwd (ADR-021 §1/§2).
func TestRunTurn_InjectsMCPConfigForButlerRole(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	bsess, err := mgr.EnsureButler("dev-1", ButlerDriver, "/ws")
	if err != nil {
		t.Fatalf("EnsureButler: %v", err)
	}

	drv := newScriptedDriver(ButlerDriver, okScript()...)
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.ButlerMCPConfig = "/cfg/butler-mcp.json"
	eng := NewEngine(deps)

	_, err = eng.RunTurn(context.Background(), Request{
		Text:             "hello",
		RequestedDriver:  ButlerDriver,
		RequestedSession: string(bsess.ID),
		DeviceID:         "dev-1",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(drv.mcpConfigs) != 1 || drv.mcpConfigs[0] != "/cfg/butler-mcp.json" {
		t.Fatalf("mcpConfigs=%v want [/cfg/butler-mcp.json]", drv.mcpConfigs)
	}
	if len(drv.cwds) != 1 || drv.cwds[0] != "/ws" {
		t.Fatalf("cwds=%v want [/ws] (butler workspace)", drv.cwds)
	}
}

// A non-butler logical session (Role=none) must NOT get --mcp-config even when
// the engine has a ButlerMCPConfig configured (worker/regular sessions never
// dispatch).
func TestRunTurn_NoMCPConfigForNonButlerRole(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	sess, err := mgr.Create("dev-1", ButlerDriver, "/proj", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	drv := newScriptedDriver(ButlerDriver, okScript()...)
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.ButlerMCPConfig = "/cfg/butler-mcp.json"
	eng := NewEngine(deps)

	_, err = eng.RunTurn(context.Background(), Request{
		Text:             "hello",
		RequestedDriver:  ButlerDriver,
		RequestedSession: string(sess.ID),
		DeviceID:         "dev-1",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(drv.mcpConfigs) != 1 || drv.mcpConfigs[0] != "" {
		t.Fatalf("mcpConfigs=%v want [\"\"] (non-butler must not get --mcp-config)", drv.mcpConfigs)
	}
}

// Butler role but no ButlerMCPConfig configured → still no --mcp-config (dispatch
// disabled gracefully).
func TestRunTurn_NoMCPConfigWhenUnconfigured(t *testing.T) {
	mgr := newTestManager(t, "/default/cwd")
	bsess, err := mgr.EnsureButler("dev-1", ButlerDriver, "/ws")
	if err != nil {
		t.Fatalf("EnsureButler: %v", err)
	}

	drv := newScriptedDriver(ButlerDriver, okScript()...)
	deps := baseDeps(routerWith(t, drv), newFakeSink(), newFakeRegistry(), Policy{})
	deps.Sessions = mgr
	deps.ButlerMCPConfig = "" // not configured
	eng := NewEngine(deps)

	_, err = eng.RunTurn(context.Background(), Request{
		Text:             "hello",
		RequestedDriver:  ButlerDriver,
		RequestedSession: string(bsess.ID),
		DeviceID:         "dev-1",
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(drv.mcpConfigs) != 1 || drv.mcpConfigs[0] != "" {
		t.Fatalf("mcpConfigs=%v want [\"\"]", drv.mcpConfigs)
	}
}
