#!/usr/bin/env bash
# butler-mcp-smoke.sh — e2e smoke for the butler dispatch MCP server (ADR-021).
#
# Two layers:
#   1. Protocol smoke (NO real claude needed): pipe initialize + tools/list +
#      list_projects into `bbclaw-adapter mcp-server` over stdio and assert the
#      JSON-RPC replies. Runs anywhere; used to sanity-check the subcommand.
#   2. Live butler smoke (requires a working `claude` CLI on PATH): start a real
#      butler `claude -p --mcp-config <cfg>` that lists projects and dispatches a
#      trivial task into the demo project. Skipped automatically when `claude`
#      is absent so CI never depends on it.
#
# Usage:
#   adapter/scripts/butler-mcp-smoke.sh            # protocol smoke only
#   BBCLAW_BUTLER_LIVE=1 adapter/scripts/butler-mcp-smoke.sh   # + live butler
set -euo pipefail

ADAPTER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BBCLAW_ADAPTER_BIN:-}"

if [[ -z "$BIN" ]]; then
  BIN="$(mktemp -d)/bbclaw-adapter"
  echo "› building bbclaw-adapter → $BIN"
  (cd "$ADAPTER_DIR" && go build -o "$BIN" ./cmd/bbclaw-adapter)
fi

DEMO_DIR="$(mktemp -d)/bbclaw-demo-project"
mkdir -p "$DEMO_DIR"
echo "# demo" > "$DEMO_DIR/README.md"

echo "› layer 1: protocol smoke over stdio (no claude required)"
OUT="$(printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}' \
  | BBCLAW_CWD_POOL="demo:$DEMO_DIR" "$BIN" mcp-server 2>/dev/null)"

echo "$OUT" | grep -q '"name":"bbclaw-butler"' || { echo "FAIL: no initialize reply"; exit 1; }
echo "$OUT" | grep -q '"name":"dispatch"'      || { echo "FAIL: tools/list missing dispatch"; exit 1; }
echo "$OUT" | grep -q "$DEMO_DIR"               || { echo "FAIL: list_projects missing demo cwd"; exit 1; }
echo "  ✓ initialize + tools/list + list_projects OK"

if [[ "${BBCLAW_BUTLER_LIVE:-0}" != "1" ]]; then
  echo "› layer 2 skipped (set BBCLAW_BUTLER_LIVE=1 to run the live butler smoke)"
  echo "PASS (protocol smoke)"
  exit 0
fi

if ! command -v claude >/dev/null 2>&1; then
  echo "› layer 2 skipped: claude CLI not on PATH"
  echo "PASS (protocol smoke; live skipped)"
  exit 0
fi

echo "› layer 2: live butler via claude -p --mcp-config"
CFG="$(mktemp).json"
cat > "$CFG" <<JSON
{
  "mcpServers": {
    "bbclaw-butler": {
      "command": "$BIN",
      "args": ["mcp-server"],
      "env": { "BBCLAW_CWD_POOL": "demo:$DEMO_DIR" }
    }
  }
}
JSON

claude -p "Use the bbclaw-butler MCP tools: call list_projects, then dispatch a task to the 'demo' project that just replies with the word READY. Report what the worker returned." \
  --mcp-config "$CFG" \
  --permission-mode acceptEdits
echo "PASS (protocol + live butler)"
