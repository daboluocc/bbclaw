package butlermcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "butler-mcp.json")

	got, err := WriteConfig(path, "/opt/bbclaw-adapter", map[string]string{
		"BBCLAW_CWD_POOL": "proj:/p",
	})
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if got != path {
		t.Fatalf("returned path=%q want %q", got, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg mcpConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entry, ok := cfg.MCPServers[ServerName]
	if !ok {
		t.Fatalf("missing %q server entry; servers=%v", ServerName, cfg.MCPServers)
	}
	if entry.Type != "stdio" {
		t.Errorf("type=%q want stdio", entry.Type)
	}
	if entry.Command != "/opt/bbclaw-adapter" {
		t.Errorf("command=%q want /opt/bbclaw-adapter", entry.Command)
	}
	if len(entry.Args) != 1 || entry.Args[0] != "mcp-server" {
		t.Errorf("args=%v want [mcp-server]", entry.Args)
	}
	if entry.Env["BBCLAW_CWD_POOL"] != "proj:/p" {
		t.Errorf("env BBCLAW_CWD_POOL=%q want proj:/p", entry.Env["BBCLAW_CWD_POOL"])
	}
}

func TestWriteConfigRejectsEmptyCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	if _, err := WriteConfig(path, "", nil); err == nil {
		t.Fatalf("WriteConfig with empty command: want error, got nil")
	}
}

func TestWriteConfigPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "butler-mcp.json")
	if _, err := WriteConfig(path, "/bin/adapter", nil); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The config can carry auth tokens, so it must not be world-readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm=%o want 600", perm)
	}
}
