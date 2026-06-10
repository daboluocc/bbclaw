package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// TestRenderButlerArgs verifies the codex -c override rendering (ADR-024 §5):
// persona → model_instructions_file temp file, dispatch server → mcp_servers.*
// overrides, and CRUCIALLY that secret env goes to procEnv (process env) not
// argv, while non-secret env goes to -c.
func TestRenderButlerArgs(t *testing.T) {
	specs := []agent.MCPServerSpec{{
		Name:    "bbclaw",
		Command: "/self",
		Args:    []string{"mcp-server"},
		Env: map[string]string{
			"BBCLAW_CWD_POOL":      "demo:/p", // non-secret → -c
			"ANTHROPIC_AUTH_TOKEN": "sk-secret", // secret → procEnv only
		},
	}}

	args, personaFile, procEnv, err := renderButlerArgs("PERSONA", specs)
	if err != nil {
		t.Fatalf("renderButlerArgs: %v", err)
	}
	if personaFile == "" {
		t.Fatal("expected persona temp file")
	}
	t.Cleanup(func() { os.Remove(personaFile) })
	if b, _ := os.ReadFile(personaFile); string(b) != "PERSONA" {
		t.Errorf("persona file=%q want PERSONA", string(b))
	}

	joined := strings.Join(args, " ")
	// Persona wired via model_instructions_file.
	if !strings.Contains(joined, "model_instructions_file=") || !strings.Contains(joined, personaFile) {
		t.Errorf("args missing model_instructions_file: %v", args)
	}
	// Dispatch server command/args present.
	if !strings.Contains(joined, `mcp_servers.bbclaw.command="/self"`) {
		t.Errorf("args missing command override: %v", args)
	}
	if !strings.Contains(joined, `mcp_servers.bbclaw.args=["mcp-server"]`) {
		t.Errorf("args missing args override: %v", args)
	}
	// Non-secret env via -c.
	if !strings.Contains(joined, `mcp_servers.bbclaw.env.BBCLAW_CWD_POOL="demo:/p"`) {
		t.Errorf("non-secret env should be in -c: %v", args)
	}
	// SECRET must NOT appear anywhere in argv.
	if strings.Contains(joined, "sk-secret") || strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("SECRET LEAKED INTO ARGV: %v", args)
	}
	// Secret must be in procEnv instead.
	if procEnv["ANTHROPIC_AUTH_TOKEN"] != "sk-secret" {
		t.Errorf("secret should be routed to procEnv, got %v", procEnv)
	}

	// Empty input → nothing.
	a, pf, pe, err := renderButlerArgs("", nil)
	if err != nil || a != nil || pf != "" || pe != nil {
		t.Errorf("empty render: args=%v file=%q env=%v err=%v", a, pf, pe, err)
	}
}

func TestCodexButlerEnabledGate(t *testing.T) {
	t.Setenv("AGENT_CODEX_BUTLER_VERIFIED", "")
	if butlerEnabled() {
		t.Error("codex butler must be disabled by default")
	}
	t.Setenv("AGENT_CODEX_BUTLER_VERIFIED", "1")
	if !butlerEnabled() {
		t.Error("codex butler must enable when AGENT_CODEX_BUTLER_VERIFIED=1")
	}
}
