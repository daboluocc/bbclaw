package butlermcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ServerName is the key the butler dispatch MCP server is registered under in
// the generated --mcp-config file. The butler addresses its tools as
// `mcp__bbclaw__dispatch`, etc.
const ServerName = "bbclaw"

// mcpServerEntry is one server entry in a claude `--mcp-config` file.
type mcpServerEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// mcpConfigFile is the top-level shape claude reads from `--mcp-config <path>`.
type mcpConfigFile struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers"`
}

// WriteConfig writes the butler's `--mcp-config` JSON file at path, pointing the
// stdio dispatch server at `<command> mcp-server` (ADR-021 §2). The butler
// session is spawned with `claude --mcp-config <path>` so it can dispatch coding
// work to worker agents through the mcp-server subcommand.
//
// env is embedded into the server entry so the spawned subprocess sees a
// deterministic project allowlist (BBCLAW_CWD_POOL / credentials) regardless of
// how the parent claude process scrubs its environment. Pass nil to rely purely
// on inherited env.
//
// The file is written atomically-enough for a single-process daemon (truncate +
// write, 0600 since it can carry auth tokens). Returns the path on success.
func WriteConfig(path, command string, env map[string]string) (string, error) {
	if command == "" {
		return "", fmt.Errorf("butlermcp: command must not be empty")
	}
	cfg := mcpConfigFile{
		MCPServers: map[string]mcpServerEntry{
			ServerName: {
				Type:    "stdio",
				Command: command,
				Args:    []string{"mcp-server"},
				Env:     env,
			},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("butlermcp: marshal mcp-config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("butlermcp: mkdir mcp-config dir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("butlermcp: write mcp-config %s: %w", path, err)
	}
	return path, nil
}
