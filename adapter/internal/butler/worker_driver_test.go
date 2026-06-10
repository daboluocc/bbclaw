package butler

import (
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// TestWithWorkerDriver verifies BBCLAW_WORKER_DRIVER is injected per butler
// driver (ADR-024 §3) WITHOUT mutating the shared spec — two devices on two
// drivers must not clobber each other's env.
func TestWithWorkerDriver(t *testing.T) {
	base := []agent.MCPServerSpec{{
		Name:    "bbclaw",
		Command: "/self",
		Args:    []string{"mcp-server"},
		Env:     map[string]string{"BBCLAW_CWD_POOL": "p:/p"},
	}}

	codexSpecs := withWorkerDriver(base, "codex")
	if got := codexSpecs[0].Env["BBCLAW_WORKER_DRIVER"]; got != "codex" {
		t.Errorf("codex: want BBCLAW_WORKER_DRIVER=codex, got %q", got)
	}
	// Original env preserved.
	if got := codexSpecs[0].Env["BBCLAW_CWD_POOL"]; got != "p:/p" {
		t.Errorf("codex: lost BBCLAW_CWD_POOL, got %q", got)
	}

	// Re-rendering for a different driver must not have mutated the first result
	// nor the shared base.
	opencodeSpecs := withWorkerDriver(base, "opencode")
	if got := opencodeSpecs[0].Env["BBCLAW_WORKER_DRIVER"]; got != "opencode" {
		t.Errorf("opencode: want BBCLAW_WORKER_DRIVER=opencode, got %q", got)
	}
	if got := codexSpecs[0].Env["BBCLAW_WORKER_DRIVER"]; got != "codex" {
		t.Errorf("codex spec mutated by opencode render: got %q", got)
	}
	if _, leaked := base[0].Env["BBCLAW_WORKER_DRIVER"]; leaked {
		t.Error("shared base spec was mutated — env map not deep-copied")
	}
}
