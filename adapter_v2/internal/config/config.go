// Package config is the minimal v2 configuration: just enough to stand up the
// PTY bridge server (Phase 1). It is a deliberately trimmed copy of the v1
// adapter/internal/config — v2 only needs three things to boot:
//
//   - Addr: the HTTP/WS listen address (distinct from v1 so both can run side
//     by side on one machine during the migration).
//   - Argv: the default CLI to launch under a PTY when a brand-new session is
//     created (e.g. "claude"). v2 runs the interactive TUI, so this must NOT
//     carry "-p"/"--output-format" flags.
//   - Cwd: the working directory for each spawned CLI process.
//
// The voice (ASR/TTS), cloud-relay and per-driver knobs from v1 are out of
// scope for Phase 1 and are reintroduced (copied or extracted into a shared
// lib) when the device channel lands — see DESIGN.md §4.
package config

import (
	"os"
	"strings"
)

// DefaultAddr is the v2 listen address. Port 18090 is intentionally distinct
// from v1's :18080 so a developer can run both adapters at once without a port
// clash (issue #207 / DESIGN.md §7).
const DefaultAddr = ":18090"

// defaultArgv is the CLI launched under a PTY for a fresh session when the
// caller supplies no explicit override. Bare "claude" runs the interactive TUI
// (no headless "-p"), which is exactly what v2's PTY transport expects.
var defaultArgv = []string{"claude"}

// Config holds the minimal settings the Phase 1 server needs.
type Config struct {
	// Addr is the HTTP/WS listen address (host:port; host may be empty for all
	// interfaces). Seeded from ADAPTER_V2_ADDR, default DefaultAddr.
	Addr string

	// Argv is the default CLI argv used by Manager.Create for a new session.
	// Argv[0] is the program; the rest are its arguments. Seeded from
	// ADAPTER_V2_CLI (space-separated), default defaultArgv.
	Argv []string

	// Cwd is the working directory for each spawned CLI process. Empty means the
	// adapter's own working directory (creack/pty inherits it). Seeded from
	// ADAPTER_V2_CWD.
	Cwd string
}

// LoadFromEnv builds a Config from the environment, falling back to the
// built-in defaults. It never fails: every field has a usable default, so a
// zero-config launch still produces a runnable server.
func LoadFromEnv() Config {
	return Config{
		Addr: getEnvOrDefault("ADAPTER_V2_ADDR", DefaultAddr),
		Argv: parseArgv(os.Getenv("ADAPTER_V2_CLI")),
		Cwd:  strings.TrimSpace(os.Getenv("ADAPTER_V2_CWD")),
	}
}

// getEnvOrDefault returns the trimmed env var value, or fallback when unset/blank.
func getEnvOrDefault(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// parseArgv splits a space-separated CLI spec into an argv slice, returning a
// copy of the built-in default when the spec is empty. Splitting on whitespace
// is sufficient for the simple commands v2 launches ("claude", "codex", ...);
// quoted arguments are not needed at this layer.
func parseArgv(spec string) []string {
	if fields := strings.Fields(spec); len(fields) > 0 {
		return fields
	}
	// Return a fresh copy so callers can't mutate the package default.
	return append([]string(nil), defaultArgv...)
}
