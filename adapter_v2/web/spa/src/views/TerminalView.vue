<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount } from "vue";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

// Live terminal over the adapter's /ws endpoint. Ported faithfully from the old
// web/index.html: fit addon, exponential-backoff reconnect, and the
// "reconnected" snapshot-replay handshake.
//
// Wire protocol (adapter_v2/internal/termchan/termchan.go):
//   client → server : {type:"input",  data}            keystrokes (utf-8)
//                      {type:"resize", cols, rows}      grid size
//   server → client : {type:"reconnected", cols, rows}  first frame on (re)connect
//                      {type:"output", data}            raw PTY bytes (ANSI)
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
let fitAddon: FitAddon | null = null;
let ws: WebSocket | null = null;
let reconnectAttempts = 0;
let reconnectTimer: number | null = null;
let destroyed = false;
let sentResize = { cols: 0, rows: 0 };
let resizeObserver: ResizeObserver | null = null;

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

function fitAndResize(force: boolean) {
  if (!term || !fitAddon) return;
  try {
    fitAddon.fit();
  } catch {
    return; // container not laid out yet
  }
  const { cols, rows } = term;
  if (cols < 2 || rows < 2) return;
  if (!force && cols === sentResize.cols && rows === sentResize.rows) return;
  sentResize = { cols, rows };
  send({ type: "resize", cols, rows });
}

function setStatus(text: string) {
  statusText.value = text;
}

function connect() {
  ws = new WebSocket(wsURL());

  ws.onopen = () => {
    reconnectAttempts = 0;
    setStatus("");
    sentResize = { cols: 0, rows: 0 };
    fitAndResize(true);
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
  fitAddon = new FitAddon();
  term.loadAddon(fitAddon);
  term.open(termHost.value!);

  term.onData((data) => send({ type: "input", data }));

  resizeObserver = new ResizeObserver(() => fitAndResize(false));
  resizeObserver.observe(termHost.value!);
  window.addEventListener("resize", onWindowResize);

  connect();
  term.focus();
});

function onWindowResize() {
  fitAndResize(false);
}

onBeforeUnmount(() => {
  destroyed = true;
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  window.removeEventListener("resize", onWindowResize);
  resizeObserver?.disconnect();
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
    <div ref="termHost" class="term-host"></div>
  </div>
</template>

<style scoped>
.term-wrap {
  position: relative;
  height: 100%;
  width: 100%;
}
.term-host {
  position: absolute;
  inset: 0;
  padding: 8px 10px;
}
.term-status {
  position: absolute;
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
/* xterm fills its host; the dot-matrix bg shows in the padding gutter. */
.term-host :deep(.xterm) {
  height: 100%;
}
.term-host :deep(.xterm-viewport) {
  background: transparent !important;
}
</style>
