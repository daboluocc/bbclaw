<script setup lang="ts">
import { ref, onMounted, computed } from "vue";
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

async function loadSessions(keepSelection = true) {
  listLoading.value = true;
  listError.value = "";
  try {
    const data = await api.listSessions();
    sessions.value = data.sessions || [];
    activeId.value = data.active || "";
    if (!keepSelection || !selected.value) {
      // default selection: active session, else first
      selectedId.value =
        activeId.value || (sessions.value[0]?.id ?? "");
      if (selectedId.value) await loadMessages(selectedId.value);
    }
  } catch (e) {
    listError.value = (e as Error).message;
  } finally {
    listLoading.value = false;
  }
}

async function loadMessages(id: string) {
  selectedId.value = id;
  msgLoading.value = true;
  msgError.value = "";
  messages.value = [];
  try {
    const data = await api.messages(id, { limit: 200 });
    messages.value = data.messages || [];
  } catch (e) {
    msgError.value = (e as Error).message;
  } finally {
    msgLoading.value = false;
  }
}

async function newSession() {
  busy.value = true;
  try {
    const { active } = await api.newSession();
    showToast("已创建新对话 New session created");
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
    await loadSessions();
  } catch (e) {
    showToast((e as Error).message, true);
  } finally {
    busy.value = false;
  }
}

onMounted(() => loadSessions(false));
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
          @click="loadMessages(s.id)"
        >
          <div class="row-title">
            {{ s.title || "(无标题 untitled)" }}
            <span v-if="s.id === activeId" class="badge">活动 active</span>
          </div>
          <div class="row-meta">
            <span class="sid">{{ shortId(s.id) }}</span>
            <span class="when">{{ fmtTime(s.lastUsedAt) }}</span>
          </div>
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
        <button
          v-if="selectedId && selectedId !== activeId"
          :disabled="busy"
          @click="activate(selectedId)"
        >
          切换到此会话 Switch
        </button>
        <span v-else-if="selectedId === activeId" class="muted">当前活动会话</span>
      </div>

      <div class="trans-body">
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
  padding: 9px 11px;
  border-radius: 7px;
  cursor: pointer;
  border: 1px solid transparent;
  margin-bottom: 4px;
}
.row:hover {
  border-color: var(--ghost);
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
.trans-head button {
  margin-left: auto;
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
