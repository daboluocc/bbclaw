package httpapi

import (
	"net/http"

	"github.com/daboluocc/bbclaw/adapter/internal/detect"
)

// Environment detection (ADR-023) backs the admin page's "which CLIs are
// actually installed" view and the `installed` flag on GET /v1/agent/drivers.
// The cache + name→installed mapping live in the detect package so the
// LAN-direct HTTP layer and the cloud relay share one source of truth.

// installedByDriver maps each known agent driver name to whether its backing
// CLI/service is present on the host. Names match Driver.Name().
func installedByDriver() map[string]bool { return detect.InstalledByDriver() }

// envRow is one driver's detection result in the GET /v1/agent/environment
// response.
type envRow struct {
	Installed bool   `json:"installed"`
	Reason    string `json:"reason,omitempty"`
}

// handleAgentEnvironment reports per-driver host detection (ADR-023 §3) so the
// admin page can show which CLIs are installed (and why not, when absent).
//
//	GET /v1/agent/environment
//	→ {"ok":true,"data":{"drivers":{
//	      "claude-code":{"installed":true},
//	      "opencode":{"installed":false,"reason":"opencode not on PATH"},
//	      ...}}}
func (s *Server) handleAgentEnvironment(w http.ResponseWriter, r *http.Request) {
	env := detect.CachedEnvironment()
	row := func(res detect.Result) envRow {
		return envRow{Installed: res.Present, Reason: res.Reason}
	}
	drivers := map[string]envRow{
		"claude-code": row(env.ClaudeCode),
		"opencode":    row(env.OpenCode),
		"aider":       row(env.Aider),
		"ollama":      row(env.Ollama),
		"openclaw":    row(env.OpenClaw),
		"codex":       row(env.Codex),
	}
	writeJSON(w, http.StatusOK, response{OK: true, Data: map[string]any{"drivers": drivers}})
}
