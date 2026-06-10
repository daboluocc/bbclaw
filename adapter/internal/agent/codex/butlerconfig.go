package codex

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// Butler support for the codex driver (ADR-024 §2/§5): translate the
// format-neutral StartOpts.SystemPrompt + MCPServers into `codex exec`
// `-c key=value` overrides. We deliberately do NOT pass --ignore-user-config:
// the butler reuses the user's native codex ecosystem (their ~/.codex MCP
// servers, AGENTS.md, etc.) and we only ADD the persona + the bbclaw dispatch
// server on top (ADR-024 §4).
//
// Persona goes to `model_instructions_file` (a per-session temp file) rather
// than the cwd AGENTS.md: the butler workspace cwd is shared across devices, so
// a per-process file avoids a multi-device race. Worker codex sessions run in
// the user's project cwd and still pick up that project's AGENTS.md natively.
//
// Secrets in the dispatch server's env are passed via the codex PROCESS env
// (inherited by the spawned mcp-server) rather than `-c ...env.X=secret`, which
// would leak them into argv / `ps`. Non-secret BBCLAW_* keys are passed via -c
// too so the mcp-server sees them even if codex scrubs its child env.

// secretEnvKey matches env keys whose values must never appear in argv.
var secretEnvKey = regexp.MustCompile(`(?i)(token|auth|secret|password|api[_-]?key)`)

// butlerEnabled gates codex as a butler backend (ADR-024 §7). The model
// emitting the dispatch tool call must be confirmed on a working codex model
// before flipping this on.
func butlerEnabled() bool {
	return os.Getenv("AGENT_CODEX_BUTLER_VERIFIED") == "1"
}

// tomlString renders a Go string as a TOML basic string for a -c value.
func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// tomlStringArray renders a string slice as a TOML array for a -c value.
func tomlStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = tomlString(it)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// renderButlerArgs returns the codex `-c` override args + the persona temp file
// path (to clean up on Stop) + the process env to set (secrets kept out of
// argv). Returns (nil,"",nil) when there is nothing to inject.
func renderButlerArgs(systemPrompt string, specs []agent.MCPServerSpec) (args []string, personaFile string, procEnv map[string]string, err error) {
	if systemPrompt == "" && len(specs) == 0 {
		return nil, "", nil, nil
	}
	if systemPrompt != "" {
		f, ferr := os.CreateTemp("", "bbclaw-codex-persona-*.md")
		if ferr != nil {
			return nil, "", nil, fmt.Errorf("codex: create persona file: %w", ferr)
		}
		if _, werr := f.WriteString(systemPrompt); werr != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, "", nil, fmt.Errorf("codex: write persona file: %w", werr)
		}
		f.Close()
		personaFile = f.Name()
		args = append(args, "-c", "model_instructions_file="+tomlString(personaFile))
	}
	for _, s := range specs {
		base := "mcp_servers." + s.Name
		args = append(args, "-c", base+".command="+tomlString(s.Command))
		if len(s.Args) > 0 {
			args = append(args, "-c", base+".args="+tomlStringArray(s.Args))
		}
		args = append(args, "-c", base+".startup_timeout_sec=30")
		args = append(args, "-c", base+".tool_timeout_sec=120")
		for k, v := range s.Env {
			if secretEnvKey.MatchString(k) {
				// Secret: route via process env only (inherited by the spawned
				// mcp-server), never argv.
				if procEnv == nil {
					procEnv = map[string]string{}
				}
				procEnv[k] = v
				continue
			}
			args = append(args, "-c", base+".env."+k+"="+tomlString(v))
		}
	}
	return args, personaFile, procEnv, nil
}
