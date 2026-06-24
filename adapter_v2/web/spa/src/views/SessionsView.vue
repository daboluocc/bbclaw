<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick, computed } from "vue";
import { api, type SessionMeta, type Message } from "../api";

const sessions = ref<SessionMeta[]>([]);
const activeId = ref<string>("");
const selectedId = ref<string>("");

const listLoading = ref(false);
const listError = ref("");

const messages = ref<Message[]>([]);
const msgLoading = ref(false);
const msgError = ref("");

const busy = ref(false); // new/activate in flight
const toast = ref<{ text: string; err: boolean } | null>(null);

// autoFollow keeps the view pinned to the live (active) conversation: the list
// + transcript refresh on a timer and the selection tracks the active session
// as new turns land. Clicking a non-active session to browse history turns it
// off; "回到实时 Live" turns it back on. Defaults on so opening #sessions shows
// the latest content and keeps updating without a manual refresh.
const autoFollow = ref(true);

const transBody = ref<HTMLElement | null>(null);

const POLL_MS = 3000;
let pollTimer: number | null = null;
// Signature of the currently-rendered transcript, so a silent poll only
// re-renders (and re-scrolls) when the messages actually changed.
let lastMsgSig = "";

let toastTimer: number | null = null;
function showToast(text: string, err = false) {
  toast.value = { text, err };
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => (toast.value = null), 2600);
}

const selected = computed(() =>
  sessions.value.find((s) => s.id === selectedId.value)
);

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function fmtTime(epochOrIso: number | string): string {
  const d =
    typeof epochOrIso === "number"
      ? new Date(epochOrIso * 1000)
      : new Date(epochOrIso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleString([], {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// ── scroll helpers ──────────────────────────────────────────────────────
function isAtBottom(): boolean {
  const el = transBody.value;
  if (!el) return true;
  return el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}
function scrollToBottomSoon() {
  // nextTick lets Vue patch the new messages in; the rAF then waits for the
  // browser to lay the (tall) transcript out before we read scrollHeight.
  // Reading it at nextTick alone can still see the empty-state height, so the
  // jump-to-latest would silently become a no-op.
  nextTick(() => {
    requestAnimationFrame(() => {
      const el = transBody.value;
      if (el) el.scrollTop = el.scrollHeight;
    });
  });
}

function msgSig(list: Message[], total: number): string {
  const last = list[list.length - 1];
  return `${total}|${list.length}|${
    last ? `${last.seq}:${last.content.length}:${last.timestamp}` : ""
  }`;
}

async function loadSessions(keepSelection = true) {
  listLoading.value = true;
  listError.value = "";
  try {
    const data = await api.listSessions();
    sessions.value = data.sessions || [];
    activeId.value = data.active || "";
    if (!keepSelection || !selected.value) {
      // default selection: active (live) session, else newest
      selectedId.value = activeId.value || (sessions.value[0]?.id ?? "");
      if (selectedId.value) await loadMessages(selectedId.value);
    }
  } catch (e) {
    listError.value = (e as Error).message;
  } finally {
    listLoading.value = false;
  }
}

// loadMessages renders a session's transcript. silent=true is the polling path:
// it never clears the view or shows a spinner, swallows transient errors, and
// only re-renders when the content changed. scroll forces a jump to the latest
// message (used on an explicit selection); otherwise it sticks to the bottom
// only when the reader is already there.
async function loadMessages(
  id: string,
  opts: { silent?: boolean; scroll?: boolean } = {}
) {
  const { silent = false } = opts;
  const scroll = opts.scroll ?? !silent;
  const switching = id !== selectedId.value;
  selectedId.value = id;
  if (!silent) {
    msgLoading.value = true;
    msgError.value = "";
    messages.value = [];
    lastMsgSig = "";
  }
  try {
    const data = await api.messages(id, { limit: 200 });
    const next = data.messages || [];
    const sig = msgSig(next, data.total);
    if (sig !== lastMsgSig || switching) {
      const stick = scroll || isAtBottom();
      messages.value = next;
      lastMsgSig = sig;
      if (stick) scrollToBottomSoon();
    }
  } catch (e) {
    if (!silent) msgError.value = (e as Error).message;
  } finally {
    if (!silent) msgLoading.value = false;
  }
}

// User picked a session from the list. Selecting the active one keeps us live;
// selecting any other one means "browse history" → stop following.
function selectSession(id: string) {
  autoFollow.value = id === activeId.value;
  loadMessages(id);
}

function goLive() {
  autoFollow.value = true;
  if (activeId.value) loadMessages(activeId.value);
}

// pollOnce is the auto-refresh tick: refresh the list, follow the active
// session when live, then silently refresh the open transcript. Skipped while a
// mutating op is in flight or the tab is hidden.
async function pollOnce() {
  if (busy.value || document.hidden) return;
  try {
    const data = await api.listSessions();
    sessions.value = data.sessions || [];
    activeId.value = data.active || "";
  } catch {
    return; // keep last-good list on a transient failure
  }
  if (
    autoFollow.value &&
    activeId.value &&
    activeId.value !== selectedId.value
  ) {
    await loadMessages(activeId.value, { silent: true, scroll: true });
    return;
  }
  if (selectedId.value) await loadMessages(selectedId.value, { silent: true });
}

function startPolling() {
  stopPolling();
  pollTimer = window.setInterval(pollOnce, POLL_MS);
}
function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}
function onVisibility() {
  if (!document.hidden) pollOnce();
}

async function newSession() {
  busy.value = true;
  try {
    const { active } = await api.newSession();
    showToast("已创建新对话 New session created");
    autoFollow.value = true;
    await loadSessions(false);
    selectedId.value = active;
    await loadMessages(active);
  } catch (e) {
    showToast((e as Error).message, true);
  } finally {
    busy.value = false;
  }
}

async function activate(id: string) {
  busy.value = true;
  try {
    await api.activate(id);
    showToast("已切换到此会话 Switched");
    autoFollow.value = true;
    await loadSessions();
  } catch (e) {
    showToast((e as Error).message, true);
  } finally {
    busy.value = false;
  }
}

// removeSession deletes a conversation (with confirmation). loadSessions(true)
// then keeps the current selection if it survived, or re-picks the new active
// when the deleted conversation was the one being viewed.
async function removeSession(id: string) {
  if (
    !window.confirm(
      "删除此会话？此操作不可撤销。\nDelete this conversation? This cannot be undone."
    )
  )
    return;
  if (id === selectedId.value) autoFollow.value = true; // land back on the live one
  busy.value = true;
  try {
    await api.deleteSession(id);
    showToast("已删除 Deleted");
    await loadSessions(true);
  } catch (e) {
    showToast((e as Error).message, true);
  } finally {
    busy.value = false;
  }
}

onMounted(async () => {
  await loadSessions(false);
  startPolling();
  document.addEventListener("visibilitychange", onVisibility);
});

onUnmounted(() => {
  stopPolling();
  document.removeEventListener("visibilitychange", onVisibility);
  if (toastTimer) clearTimeout(toastTimer);
});
</script>

<template>
  <div class="sessions">
    <!-- left: session list -->
    <aside class="sidebar">
      <div class="side-head">
        <h2>会话 Sessions</h2>
        <div class="side-actions">
          <button class="small" :disabled="busy" @click="newSession">
            + 新对话 New
          </button>
          <button
            class="ghost small"
            :disabled="listLoading"
            title="刷新 Refresh"
            @click="loadSessions(false)"
          >
            ↻
          </button>
        </div>
      </div>

      <div v-if="listLoading && !sessions.length" class="state">载入中 Loading…</div>
      <div v-else-if="listError" class="state err">错误 {{ listError }}</div>
      <div v-else-if="!sessions.length" class="state">
        暂无会话 No sessions yet
      </div>

      <ul v-else class="slist">
        <li
          v-for="s in sessions"
          :key="s.id"
          class="row"
          :class="{ sel: s.id === selectedId }"
          @click="selectSession(s.id)"
        >
          <div class="row-title">
            {{ s.title || "(无标题 untitled)" }}
            <span v-if="s.id === activeId" class="badge">活动 active</span>
          </div>
          <div class="row-meta">
            <span class="sid">{{ shortId(s.id) }}</span>
            <span class="when">{{ fmtTime(s.lastUsedAt) }}</span>
          </div>
          <button
            class="del"
            :disabled="busy"
            title="删除会话 Delete"
            @click.stop="removeSession(s.id)"
          >
            ✕
          </button>
        </li>
      </ul>
    </aside>

    <!-- right: transcript -->
    <section class="transcript">
      <div class="trans-head">
        <div class="th-left">
          <h2>对话记录 Transcript</h2>
          <span v-if="selected" class="muted">
            {{ selected.title || shortId(selected.id) }}
          </span>
        </div>
        <div class="th-right">
          <span v-if="autoFollow" class="live" title="自动刷新中 Auto-refreshing">
            <span class="dot"></span>实时 Live
          </span>
          <button
            v-else
            class="ghost small"
            title="回到实时并自动刷新 Follow the live session"
            @click="goLive"
          >
            ↺ 回到实时 Live
          </button>
          <button
            v-if="selectedId && selectedId !== activeId"
            :disabled="busy"
            @click="activate(selectedId)"
          >
            切换到此会话 Switch
          </button>
        </div>
      </div>

      <div ref="transBody" class="trans-body">
        <div v-if="!selectedId" class="state">
          ← 选择一个会话 Select a session
        </div>
        <div v-else-if="msgLoading" class="state">载入中 Loading…</div>
        <div v-else-if="msgError" class="state err">错误 {{ msgError }}</div>
        <div v-else-if="!messages.length" class="state">
          暂无消息 No messages
        </div>
        <div v-else class="bubbles">
          <div
            v-for="m in messages"
            :key="m.seq"
            class="bubble"
            :class="m.role"
          >
            <div class="bub-head">
              <span class="role">{{
                m.role === "user" ? "用户 user" : "助手 assistant"
              }}</span>
              <span class="ts">{{ fmtTime(m.timestamp) }}</span>
            </div>
            <pre class="content">{{ m.content }}</pre>
          </div>
        </div>
      </div>
    </section>

    <div v-if="toast" class="toast" :class="{ err: toast.err }">
      {{ toast.text }}
    </div>
  </div>
</template>

<style scoped>
.sessions {
  display: grid;
  grid-template-columns: 300px 1fr;
  gap: 0;
  height: 100%;
  min-height: 0;
}

/* ── sidebar ─────────────────────────────────────────────────────────── */
.sidebar {
  border-right: 1px solid var(--ghost);
  display: flex;
  flex-direction: column;
  min-height: 0;
  background: rgba(7, 11, 14, 0.4);
}
.side-head {
  padding: 14px 16px 10px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  border-bottom: 1px solid var(--ghost);
}
.side-actions {
  display: flex;
  gap: 8px;
}
.side-actions button:first-child {
  flex: 1;
}
.slist {
  list-style: none;
  margin: 0;
  padding: 6px;
  overflow-y: auto;
  flex: 1;
  min-height: 0;
}
.row {
  position: relative;
  padding: 9px 30px 9px 11px;
  border-radius: 7px;
  cursor: pointer;
  border: 1px solid transparent;
  margin-bottom: 4px;
}
.row:hover {
  border-color: var(--ghost);
}
.del {
  position: absolute;
  top: 50%;
  right: 5px;
  transform: translateY(-50%);
  font: inherit;
  font-size: 11px;
  line-height: 1;
  padding: 4px 7px;
  border-radius: 5px;
  border: 1px solid transparent;
  background: transparent;
  color: var(--dim);
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.12s;
}
.row:hover .del,
.row.sel .del {
  opacity: 0.65;
}
.del:hover:not(:disabled) {
  opacity: 1;
  color: var(--err);
  border-color: var(--err);
  background: rgba(230, 111, 111, 0.08);
}
.del:disabled {
  cursor: default;
}
.row.sel {
  background: rgba(46, 196, 160, 0.1);
  box-shadow: inset 3px 0 0 var(--accent);
  color: var(--lit);
}
.row-title {
  font-size: 12.5px;
  color: var(--lit);
  display: flex;
  align-items: center;
  gap: 8px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.badge {
  font-size: 9px;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: #04110f;
  background: var(--accent);
  border-radius: 4px;
  padding: 1px 6px;
  flex: none;
}
.row-meta {
  display: flex;
  gap: 10px;
  margin-top: 4px;
  font-size: 10.5px;
  color: var(--dim);
}
.sid {
  font-family: ui-monospace, monospace;
}
.when {
  margin-left: auto;
}

/* ── transcript ──────────────────────────────────────────────────────── */
.transcript {
  display: flex;
  flex-direction: column;
  min-height: 0;
  min-width: 0;
}
.trans-head {
  padding: 14px 18px;
  border-bottom: 1px solid var(--ghost);
  display: flex;
  align-items: center;
  gap: 14px;
}
.th-left {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.th-right {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 10px;
  flex: none;
}
.live {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--accent);
}
.live .dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--accent);
  box-shadow: 0 0 0 0 rgba(46, 196, 160, 0.55);
  animation: live-pulse 1.8s ease-out infinite;
}
@keyframes live-pulse {
  0% {
    box-shadow: 0 0 0 0 rgba(46, 196, 160, 0.55);
  }
  70% {
    box-shadow: 0 0 0 6px rgba(46, 196, 160, 0);
  }
  100% {
    box-shadow: 0 0 0 0 rgba(46, 196, 160, 0);
  }
}
.trans-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 18px;
}
.bubbles {
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-width: 880px;
}
.bubble {
  border: 1px solid var(--ghost);
  border-radius: 10px;
  padding: 10px 14px;
  background: rgba(13, 18, 22, 0.78);
}
.bubble.user {
  align-self: flex-end;
  max-width: 85%;
  box-shadow: inset 3px 0 0 var(--dim);
}
.bubble.assistant {
  align-self: flex-start;
  max-width: 92%;
  box-shadow: inset 3px 0 0 var(--accent);
}
.bub-head {
  display: flex;
  gap: 10px;
  align-items: baseline;
  margin-bottom: 6px;
}
.role {
  font-size: 10px;
  letter-spacing: 0.1em;
  text-transform: uppercase;
  color: var(--dim);
}
.bubble.assistant .role {
  color: var(--accent);
}
.ts {
  margin-left: auto;
  font-size: 10px;
  color: var(--dim);
  opacity: 0.7;
}
.content {
  margin: 0;
  font: 12.5px/1.55 ui-monospace, SFMono-Regular, Menlo, monospace;
  color: var(--lit);
  white-space: pre-wrap;
  word-break: break-word;
}

/* ── states ──────────────────────────────────────────────────────────── */
.state {
  color: var(--dim);
  padding: 18px;
  font-size: 12px;
}
.state.err {
  color: var(--err);
}

@media (max-width: 720px) {
  .sessions {
    grid-template-columns: 1fr;
    grid-template-rows: auto 1fr;
  }
  .sidebar {
    border-right: 0;
    border-bottom: 1px solid var(--ghost);
    max-height: 40vh;
  }
}
</style>
