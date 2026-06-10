package butlermcp

// ServerName is the key the butler dispatch MCP server is registered under
// (ADR-021 §2). The butler addresses its tools as `mcp__bbclaw__dispatch` on
// claude, `bbclaw_dispatch` on opencode, etc. — the per-driver tool-name shape
// derives from this name.
//
// The actual config rendering — claude's --mcp-config JSON, codex's
// `-c mcp_servers.*` overrides, opencode's OPENCODE_CONFIG_CONTENT mcp block —
// lives in each driver, fed by the format-neutral agent.MCPServerSpec
// (ADR-024 §5). This package no longer owns a claude-specific config writer.
const ServerName = "bbclaw"
