package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// claude --mcp-config accepts JSON files OR strings, but the butler dispatch
// server's env carries credentials (ANTHROPIC_AUTH_TOKEN etc.), so we must
// render to a 0600 FILE rather than an inline argv string (which would leak
// secrets via ps). This is claude's own renderer for agent.MCPServerSpec
// (ADR-024 §5): the format-neutral spec → claude's {mcpServers:{...}} shape.
//
// We cannot reuse internal/butlermcp's renderer here because butlermcp imports
// claudecode (runner_claude.go), so the dependency would be circular. The
// shape is small; duplicating it is cheaper than the import gymnastics.

type claudeMCPEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type claudeMCPFile struct {
	MCPServers map[string]claudeMCPEntry `json:"mcpServers"`
}

// renderMCPConfigFile renders specs into a claude --mcp-config JSON file and
// returns its path. Files are cached per unique content (keyed by a content
// hash) so repeated butler sessions sharing the same dispatch server reuse one
// file instead of churning the disk. Returns "" for an empty spec.
func (d *Driver) renderMCPConfigFile(specs []agent.MCPServerSpec) (string, error) {
	if len(specs) == 0 {
		return "", nil
	}
	servers := make(map[string]claudeMCPEntry, len(specs))
	for _, s := range specs {
		servers[s.Name] = claudeMCPEntry{
			Type:    "stdio",
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
		}
	}
	data, err := json.MarshalIndent(claudeMCPFile{MCPServers: servers}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("claudecode: marshal mcp-config: %w", err)
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:8])

	d.mcpMu.Lock()
	defer d.mcpMu.Unlock()
	if d.mcpFiles == nil {
		d.mcpFiles = make(map[string]string)
	}
	if p, ok := d.mcpFiles[key]; ok {
		return p, nil
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("bbclaw-claude-mcp-%d-%s.json", os.Getpid(), key))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("claudecode: write mcp-config %s: %w", path, err)
	}
	d.mcpFiles[key] = path
	return path, nil
}
