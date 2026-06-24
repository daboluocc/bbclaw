<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount } from "vue";
import { Terminal } from "@xterm/xterm";

// Live terminal over the adapter's /ws endpoint.
//
// FIXED-SIZE VIEWER (ADR-035): the default session's PTY is pinned to a fixed,
// generous grid (session.DefaultGridCols×Rows) and is NEVER resized at runtime —
// the device line screen-scrapes claude's TUI off it, and a browser-driven
// shrink would push a tall reply's top off the visible grid and starve
// extraction (see extract/CASES.md C9). So this view does NOT fit/resize the
// PTY to the browser: it sizes its xterm to the grid the server reports in the
// "reconnected" frame, and the CSS frame (.term-wrap, overflow:auto) scrolls
// that fixed-size terminal into the panel. No fit addon, no resize messages.
//
// Wire protocol (adapter_v2/internal/termchan/termchan.go):
//   client → server : {type:"input",  data}             keystrokes (utf-8)
//   server → client : {type:"reconnected", cols, rows}   grid size on (re)connect
//                      {type:"output", data}             raw PTY bytes (ANSI)
//
// On (re)connect the server replays scrollback + a screen snapshot as a burst
// of "output" frames; we reset() the terminal on "reconnected" so the ANSI
// replay paints onto a clean grid.

const termHost = ref<HTMLDivElement | null>(null);
const statusText = ref("");

// ?session=<id> selects a specific session; empty joins the shared default
// (device) session — same resolution as the old client.
function resolveSessionId(): string {
  return new URLSearchParams(location.search).get("session") || "";
}
const sessionId = resolveSessionId();

let term: Terminal | null = null;
let ws: WebSocket | null = null;
let reconnectAttempts = 0;
let reconnectTimer: number | null = null;
let destroyed = false;

function wsURL(): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const base = `${proto}//${location.host}/ws`;
  return sessionId ? `${base}?session=${encodeURIComponent(sessionId)}` : base;
}

function send(msg: unknown) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

function setStatus(text: string) {
  statusText.value = text;
}

function connect() {
  ws = new WebSocket(wsURL());

  ws.onopen = () => {
    reconnectAttempts = 0;
    setStatus("");
    // No resize handshake: the server owns the (fixed) grid and announces it in
    // the "reconnected" frame below.
  };

  ws.onmessage = (ev) => {
    let msg: { type?: string; data?: string; cols?: number; rows?: number };
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return; // ignore non-JSON frames
    }
    switch (msg.type) {
      case "reconnected":
        term?.reset();
        // Size our xterm to the server's FIXED grid (we never drive it the other
        // way). The CSS frame scrolls it if it overflows the panel.
        if (msg.cols && msg.rows && msg.cols >= 2 && msg.rows >= 2) {
          term?.resize(msg.cols, msg.rows);
        }
        break;
      case "output":
        if (typeof msg.data === "string") term?.write(msg.data);
        break;
      default:
        break; // unknown type: ignore for forward compatibility
    }
  };

  ws.onclose = (ev) => {
    if (destroyed) return;
    if (ev.code === 1000) {
      setStatus("session ended");
      term?.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
      return;
    }
    scheduleReconnect();
  };

  ws.onerror = () => {
    // onclose handles recovery; nothing actionable here.
  };
}

function scheduleReconnect() {
  if (destroyed || reconnectTimer) return;
  const delay = Math.min(1000 * 2 ** reconnectAttempts, 30000);
  reconnectAttempts += 1;
  setStatus(`连接已断开 — ${Math.round(delay / 1000)}s 后重连…`);
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, delay);
}

onMounted(() => {
  term = new Terminal({
    cursorBlink: true,
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
    fontSize: 14,
    allowProposedApi: true,
    // Calm dark scheme tuned to the dot-matrix palette; a complete 16-color
    // ANSI set so claude's light input field doesn't paint as a near-white bar.
    theme: {
      background: "#070b0e",
      foreground: "#dfeaec",
      cursor: "#2ec4a0",
      cursorAccent: "#070b0e",
      selectionBackground: "#152128",
      black: "#070b0e",
      red: "#e66f6f",
      green: "#4cd964",
      yellow: "#e5c07b",
      blue: "#56b6c2",
      magenta: "#2ec4a0",
      cyan: "#2ec4a0",
      white: "#dfeaec",
      brightBlack: "#6e8a93",
      brightRed: "#e66f6f",
      brightGreen: "#4cd964",
      brightYellow: "#e5c07b",
      brightBlue: "#56b6c2",
      brightMagenta: "#2ec4a0",
      brightCyan: "#2ec4a0",
      brightWhite: "#ffffff",
    },
  });
  term.open(termHost.value!);

  term.onData((data) => send({ type: "input", data }));

  connect();
  term.focus();
});

onBeforeUnmount(() => {
  destroyed = true;
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  if (ws) {
    try {
      ws.close(1000);
    } catch {
      /* ignore */
    }
  }
  term?.dispose();
  term = null;
});
</script>

<template>
  <div class="term-wrap">
    <div v-if="statusText" class="term-status">{{ statusText }}</div>
    <!-- Fixed-size terminal; .term-wrap frames + scrolls it (ADR-035). -->
    <div ref="termHost" class="term-host"></div>
  </div>
</template>

<style scoped>
.term-wrap {
  position: relative;
  height: 100%;
  width: 100%;
  overflow: auto; /* frame: scroll the fixed-size grid into view */
}
.term-host {
  width: max-content; /* shrink-wrap to the terminal's natural grid width */
  padding: 8px 10px;
}
.term-status {
  position: sticky;
  top: 0;
  left: 0;
  right: 0;
  z-index: 10;
  padding: 6px 12px;
  font: 12px/1.4 ui-monospace, monospace;
  color: #04110f;
  background: var(--accent);
  text-align: center;
  letter-spacing: 0.05em;
}
.term-host :deep(.xterm-viewport) {
  background: transparent !important;
  overflow: visible !important; /* outer .term-wrap owns scrolling */
}
</style>
