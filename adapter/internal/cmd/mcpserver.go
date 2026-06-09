package cmd

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/butler/memory"
	"github.com/daboluocc/bbclaw/adapter/internal/butlermcp"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/daboluocc/bbclaw/adapter/internal/projectstore"
	"github.com/daboluocc/bbclaw/adapter/internal/workspace"
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

	// The allow-list is the union of the env-defined pool (seed) and the
	// admin-added delta persisted by the main adapter process. Reading the shared
	// projectstore file keeps this subprocess live: a project the user adds
	// through the local admin page is honoured on the next list_projects /
	// dispatch without restarting the butler. A store open failure degrades to
	// the env-only pool rather than aborting dispatch.
	var projectsFn func() []butlermcp.Project
	if dataDir, derr := workspace.DataDir(); derr != nil {
		logger.Warnf("mcp-server: resolve data dir failed, using env-only pool: %v", derr)
	} else {
		storePath := filepath.Join(dataDir, projectsFileName)
		// Bootstrap too, in case this subprocess somehow runs before the main
		// process has created/migrated the file; a no-op once it's current-format.
		_, _ = projectstore.Bootstrap(storePath, cwdPoolToSeed(cfg.CwdPool))
		if store, oerr := projectstore.Open(storePath); oerr != nil {
			logger.Warnf("mcp-server: open project store failed, using env-only pool: %v", oerr)
		} else {
			projectsFn = func() []butlermcp.Project { return storeToProjects(store.List()) }
		}
	}

	seed := cwdPoolToProjects(cfg.CwdPool)
	if projectsFn == nil && len(seed) == 0 {
		logger.Warnf("mcp-server: no projects configured (set BBCLAW_CWD_POOL or BBCLAW_DEFAULT_CWD, or add one in the admin page); dispatch will reject all calls")
	}

	runner := butlermcp.NewClaudeWorkerRunner(butlermcp.ClaudeRunnerOptions{
		Bin:       os.Getenv("AGENT_CLAUDE_CODE_BIN"),
		BaseURL:   cfg.ClaudeBaseURL,
		AuthToken: cfg.ClaudeAuthToken,
		ExtraArgs: parseArgList(os.Getenv("AGENT_CLAUDE_CODE_EXTRA_ARGS")),
		Logger:    logger,
	})

	srv := butlermcp.New(butlermcp.Options{
		Projects:         seed, // static fallback when the store could not be opened
		ProjectsProvider: projectsFn,
		Runner:           runner,
		Log:              logger,
		MemoryWriter:     resolveMemoryWriter(logger),
	})

	logger.Infof("mcp-server: serving on stdio (env seed: %d project(s))", len(seed))
	return srv.Serve(os.Stdin, os.Stdout)
}

// projectsFileName is the admin-managed project delta, co-located with the other
// adapter state under the data directory. Shared with the main process.
const projectsFileName = "projects.json"

// cwdPoolToSeed maps the parsed env pool into projectstore seed entries.
func cwdPoolToSeed(pool []config.CwdEntry) []projectstore.Project {
	out := make([]projectstore.Project, 0, len(pool))
	for _, e := range pool {
		out = append(out, projectstore.Project{Name: e.Name, Path: e.Path})
	}
	return out
}

// storeToProjects adapts projectstore entries into butlermcp projects.
func storeToProjects(in []projectstore.Project) []butlermcp.Project {
	out := make([]butlermcp.Project, 0, len(in))
	for _, p := range in {
		out = append(out, butlermcp.Project{Name: p.Name, Cwd: p.Path})
	}
	return out
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

// resolveMemoryWriter returns a MemoryWriter when the butler memory pipeline is
// enabled (BBCLAW_BUTLER_MEMORY_DISTILL=1), otherwise nil. A nil MemoryWriter
// causes the `remember` MCP tool to return REMEMBER_UNAVAILABLE so the butler
// gets a clear signal rather than silently dropping notes.
func resolveMemoryWriter(log *obs.Logger) butlermcp.MemoryWriter {
	if !memory.Enabled() {
		return nil
	}
	// EnsureScaffold guarantees the MEMORY/ directory and skeleton files exist
	// before the butler session starts writing. A failure here is non-fatal:
	// WriteMemory will create the directory itself on first write.
	if _, err := workspace.EnsureScaffold(); err != nil {
		if log != nil {
			log.Warnf("mcp-server: workspace scaffold failed (non-fatal): %v", err)
		}
	}
	return butlermcp.NewWorkspaceMemoryWriter()
}
