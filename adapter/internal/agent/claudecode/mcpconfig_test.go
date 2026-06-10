package claudecode

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// TestRenderMCPConfigFile checks the spec → claude --mcp-config JSON rendering
// (ADR-024 §5): correct shape, 0600 perms (carries auth tokens), and content
// caching (same spec reuses one file).
func TestRenderMCPConfigFile(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	specs := []agent.MCPServerSpec{{
		Name:    "bbclaw",
		Command: "/opt/bbclaw-adapter",
		Args:    []string{"mcp-server"},
		Env:     map[string]string{"BBCLAW_CWD_POOL": "proj:/p"},
	}}

	path, err := d.renderMCPConfigFile(specs)
	if err != nil {
		t.Fatalf("renderMCPConfigFile: %v", err)
	}
	if path == "" {
		t.Fatal("expected a path, got empty")
	}
	t.Cleanup(func() { os.Remove(path) })

	// Permissions: must not be world-readable (carries tokens).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm=%o want 600", perm)
	}

	// Shape: claude {mcpServers:{bbclaw:{type:stdio,command,args,env}}}.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cfg claudeMCPFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entry, ok := cfg.MCPServers["bbclaw"]
	if !ok {
		t.Fatalf("missing bbclaw entry; servers=%v", cfg.MCPServers)
	}
	if entry.Type != "stdio" || entry.Command != "/opt/bbclaw-adapter" {
		t.Errorf("entry=%+v want stdio /opt/bbclaw-adapter", entry)
	}
	if len(entry.Args) != 1 || entry.Args[0] != "mcp-server" {
		t.Errorf("args=%v want [mcp-server]", entry.Args)
	}
	if entry.Env["BBCLAW_CWD_POOL"] != "proj:/p" {
		t.Errorf("env=%v want BBCLAW_CWD_POOL=proj:/p", entry.Env)
	}

	// Caching: identical spec returns the same path; empty spec returns "".
	path2, _ := d.renderMCPConfigFile(specs)
	if path2 != path {
		t.Errorf("expected cached path %q, got %q", path, path2)
	}
	if empty, _ := d.renderMCPConfigFile(nil); empty != "" {
		t.Errorf("empty spec should render no file, got %q", empty)
	}
}
