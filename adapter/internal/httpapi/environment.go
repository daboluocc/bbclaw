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

// warningsByDriver maps a driver name to a non-fatal advisory derived from
// detection — currently the opencode serve version-support warning (ADR-031
// P2-5). Empty when there is nothing to warn about.
func warningsByDriver() map[string]string {
	env := detect.CachedEnvironment()
	out := map[string]string{}
	if w, ok := env.OpenCode.Data["serveVersionWarning"].(string); ok && w != "" {
		out["opencode"] = w
	}
	return out
}

// envRow is one driver's detection result in the GET /v1/agent/environment
// response.
type envRow struct {
	Installed bool   `json:"installed"`
	Reason    string `json:"reason,omitempty"`
	// Version is the detected CLI version when known. Warning is a non-fatal
	// advisory shown even when installed — e.g. an opencode version outside the
	// serve backend's supported range (ADR-031 P2-5).
	Version string `json:"version,omitempty"`
	Warning string `json:"warning,omitempty"`
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
		r := envRow{Installed: res.Present, Reason: res.Reason}
		if v, ok := res.Data["version"].(string); ok {
			r.Version = v
		}
		if w, ok := res.Data["serveVersionWarning"].(string); ok {
			r.Warning = w
		}
		return r
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
