package cmd

import (
	"os"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/butlermcp"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/spf13/cobra"
)

// NewMcpServerCmd creates the `mcp-server` subcommand: the stdio MCP server the
// conversational orchestrator butler talks to (ADR-021). It is launched by the
// butler as `claude -p --mcp-config <cfg>`, where <cfg> points the command at
// this subcommand. stdout carries ONLY JSON-RPC; all logs go to stderr.
func NewMcpServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp-server",
		Short: "Run the butler dispatch MCP server on stdio (ADR-021)",
		Long: `Run the stdio MCP server the conversational butler dispatches through.

Exposes list_projects / dispatch / task_status / task_result over newline-
delimited JSON-RPC 2.0 on stdin/stdout. Worker tasks run as claude-code
sessions in the allow-listed project directories from BBCLAW_CWD_POOL.

stdout is reserved for JSON-RPC — all logging is emitted to stderr.

Configuration (env):
  BBCLAW_CWD_POOL          name:path,name:path,…  allow-listed projects
  BBCLAW_DEFAULT_CWD       single-project fallback when CWD_POOL is unset
  ANTHROPIC_BASE_URL       custom API base for worker claude sessions
  ANTHROPIC_AUTH_TOKEN     auth token for the custom endpoint
  AGENT_CLAUDE_CODE_BIN    path to the claude binary (default: PATH lookup)
  AGENT_CLAUDE_CODE_EXTRA_ARGS  extra args appended to the worker invocation`,
		RunE: runMcpServer,
	}
}

func runMcpServer(cmd *cobra.Command, args []string) error {
	// The butler MCP server only dispatches coding workers — it never touches
	// the ASR/TTS pipeline — so it reads just the project allowlist + claude
	// credentials and skips the full adapter's voice-path validation.
	cfg := config.LoadButlerEnv()

	// Logger MUST write to stderr — stdout is the JSON-RPC channel.
	logger := obs.NewLoggerTo(os.Stderr)

	projects := cwdPoolToProjects(cfg.CwdPool)
	if len(projects) == 0 {
		logger.Warnf("mcp-server: no projects configured (set BBCLAW_CWD_POOL or BBCLAW_DEFAULT_CWD); dispatch will reject all calls")
	}

	runner := butlermcp.NewClaudeWorkerRunner(butlermcp.ClaudeRunnerOptions{
		Bin:       os.Getenv("AGENT_CLAUDE_CODE_BIN"),
		BaseURL:   cfg.ClaudeBaseURL,
		AuthToken: cfg.ClaudeAuthToken,
		ExtraArgs: parseArgList(os.Getenv("AGENT_CLAUDE_CODE_EXTRA_ARGS")),
		Logger:    logger,
	})

	srv := butlermcp.New(butlermcp.Options{
		Projects: projects,
		Runner:   runner,
		Log:      logger,
	})

	logger.Infof("mcp-server: serving %d project(s) on stdio", len(projects))
	return srv.Serve(os.Stdin, os.Stdout)
}

// cwdPoolToProjects maps the parsed CWD pool into butlermcp projects. The two
// types carry the same data under different field names (CwdEntry{Name,Path} vs
// Project{Name,Cwd}); this is the single adapter point.
func cwdPoolToProjects(pool []config.CwdEntry) []butlermcp.Project {
	if len(pool) == 0 {
		return nil
	}
	out := make([]butlermcp.Project, 0, len(pool))
	for _, e := range pool {
		out = append(out, butlermcp.Project{Name: e.Name, Cwd: e.Path})
	}
	return out
}

// parseArgList splits a comma-separated arg string into a slice, mirroring the
// behaviour the main binary uses for AGENT_*_EXTRA_ARGS. Empty input yields nil.
func parseArgList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
