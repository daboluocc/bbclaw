package opencode

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// TestRenderButlerConfig verifies the OPENCODE_CONFIG_CONTENT rendering
// (ADR-024 §5): persona → instructions file, dispatch server → mcp block with
// command/env, and the empty case.
func TestRenderButlerConfig(t *testing.T) {
	specs := []agent.MCPServerSpec{{
		Name:    "bbclaw",
		Command: "/self",
		Args:    []string{"mcp-server"},
		Env:     map[string]string{"BBCLAW_CWD_POOL": "p:/p"},
	}}

	content, personaFile, err := renderButlerConfig("PERSONA-TEXT", specs)
	if err != nil {
		t.Fatalf("renderButlerConfig: %v", err)
	}
	if personaFile == "" {
		t.Fatal("expected a persona temp file")
	}
	t.Cleanup(func() { os.Remove(personaFile) })

	// Persona file holds the system prompt verbatim.
	if b, _ := os.ReadFile(personaFile); string(b) != "PERSONA-TEXT" {
		t.Errorf("persona file=%q want PERSONA-TEXT", string(b))
	}

	var cfg ocConfig
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if len(cfg.Instructions) != 1 || cfg.Instructions[0] != personaFile {
		t.Errorf("instructions=%v want [%s]", cfg.Instructions, personaFile)
	}
	srv, ok := cfg.MCP["bbclaw"]
	if !ok {
		t.Fatalf("missing bbclaw mcp server; mcp=%v", cfg.MCP)
	}
	if srv.Type != "local" || !srv.Enabled {
		t.Errorf("server=%+v want type=local enabled=true", srv)
	}
	if len(srv.Command) != 2 || srv.Command[0] != "/self" || srv.Command[1] != "mcp-server" {
		t.Errorf("command=%v want [/self mcp-server]", srv.Command)
	}
	if srv.Environment["BBCLAW_CWD_POOL"] != "p:/p" {
		t.Errorf("env=%v want BBCLAW_CWD_POOL=p:/p", srv.Environment)
	}

	// Empty input → no config, no file.
	c, pf, err := renderButlerConfig("", nil)
	if err != nil || c != "" || pf != "" {
		t.Errorf("empty render: got content=%q file=%q err=%v", c, pf, err)
	}
}

// TestButlerEnabledGate documents the opt-in gate (ADR-024 §7).
func TestButlerEnabledGate(t *testing.T) {
	t.Setenv("AGENT_OPENCODE_BUTLER_VERIFIED", "")
	if butlerEnabled() {
		t.Error("butler must be disabled by default")
	}
	t.Setenv("AGENT_OPENCODE_BUTLER_VERIFIED", "1")
	if !butlerEnabled() {
		t.Error("butler must enable when AGENT_OPENCODE_BUTLER_VERIFIED=1")
	}
}
