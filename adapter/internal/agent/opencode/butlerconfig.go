package opencode

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// Butler support for the opencode driver (ADR-024 §2/§5): translate the
// format-neutral StartOpts.SystemPrompt + MCPServers into opencode's own config
// via the OPENCODE_CONFIG_CONTENT env var, which opencode MERGES on top of the
// user's global ~/.config/opencode config. Merging is intentional (ADR-024 §4):
// the butler reuses the user's native opencode ecosystem (skills, their own MCP
// servers) and we only ADD the persona instructions + the bbclaw dispatch
// server. Tool names surface as `bbclaw_dispatch` (server_tool), unlike claude's
// mcp__bbclaw__dispatch.
//
// Env-var carriage (not argv) keeps the dispatch server's embedded credentials
// out of the process arg list.

type ocMCPServer struct {
	Type        string            `json:"type"` // "local" for stdio
	Command     []string          `json:"command"`
	Enabled     bool              `json:"enabled"`
	Environment map[string]string `json:"environment,omitempty"`
}

type ocConfig struct {
	Instructions []string               `json:"instructions,omitempty"`
	MCP          map[string]ocMCPServer `json:"mcp,omitempty"`
}

// butlerEnabled gates opencode as a butler backend (ADR-024 §7). The model
// actually emitting the dispatch tool call could not be verified end-to-end on
// the dev box (model creds), so Capabilities.Butler stays false until the
// operator confirms it on a working model and opts in.
func butlerEnabled() bool {
	return os.Getenv("AGENT_OPENCODE_BUTLER_VERIFIED") == "1"
}

// renderButlerConfig builds the OPENCODE_CONFIG_CONTENT JSON from the persona
// and the dispatch MCP servers. systemPrompt, when non-empty, is written to a
// temp file referenced via `instructions` (opencode appends instruction-file
// contents to the system prompt). Returns the JSON content and the persona temp
// file path (to clean up on Stop), or ("","") when there is nothing to inject.
func renderButlerConfig(systemPrompt string, specs []agent.MCPServerSpec) (content, personaFile string, err error) {
	var cfg ocConfig
	if systemPrompt != "" {
		f, ferr := os.CreateTemp("", "bbclaw-opencode-persona-*.md")
		if ferr != nil {
			return "", "", fmt.Errorf("opencode: create persona file: %w", ferr)
		}
		if _, werr := f.WriteString(systemPrompt); werr != nil {
			f.Close()
			os.Remove(f.Name())
			return "", "", fmt.Errorf("opencode: write persona file: %w", werr)
		}
		f.Close()
		personaFile = f.Name()
		cfg.Instructions = []string{personaFile}
	}
	if len(specs) > 0 {
		cfg.MCP = make(map[string]ocMCPServer, len(specs))
		for _, s := range specs {
			cfg.MCP[s.Name] = ocMCPServer{
				Type:        "local",
				Command:     append([]string{s.Command}, s.Args...),
				Enabled:     true,
				Environment: s.Env,
			}
		}
	}
	if cfg.Instructions == nil && cfg.MCP == nil {
		return "", "", nil
	}
	data, merr := json.Marshal(cfg)
	if merr != nil {
		if personaFile != "" {
			os.Remove(personaFile)
		}
		return "", "", fmt.Errorf("opencode: marshal config: %w", merr)
	}
	return string(data), personaFile, nil
}
