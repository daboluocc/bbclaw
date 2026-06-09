<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { listSessions, sessionMessages, dispatchRecent, type SessionInfo, type ChatMessage, type DispatchEntry } from "../api";

// Split the transcript into time segments: a gap larger than this between two
// consecutive messages starts a new segment with a divider.
const GAP_MS = 30 * 60 * 1000;

const sessions = ref<SessionInfo[]>([]);
const activeId = ref<string | null>(null);
const activeSession = ref<SessionInfo | null>(null);
const messages = ref<ChatMessage[]>([]);
const dispatches = ref<DispatchEntry[]>([]);
const loadingMsgs = ref(false);
const err = ref("");
let timer: number | undefined;

function clock(s?: string) {
  if (!s) return "";
  const d = new Date(s);
  return isNaN(+d) ? "" : d.toLocaleString();
}

// segments groups messages into time-separated runs; each run carries a label
// (the start time of the run) plus its messages. A gap > GAP_MS, or any
// transition involving a timestamp-less message, starts a new run.
const segments = computed(() => {
  const runs: { label: string; items: ChatMessage[] }[] = [];
  let prevTs = NaN;
  for (const m of messages.value) {
    const t = m.timestamp ? new Date(m.timestamp).getTime() : NaN;
    const split = runs.length === 0 || (!isNaN(t) && !isNaN(prevTs) && t - prevTs > GAP_MS) || (isNaN(t) !== isNaN(prevTs));
    if (split) {
      runs.push({ label: m.timestamp ? new Date(m.timestamp).toLocaleString() : "未记录时间", items: [] });
    }
    runs[runs.length - 1].items.push(m);
    prevTs = t;
  }
  return runs;
});

async function loadSessions() {
  try {
    sessions.value = await listSessions();
    err.value = "";
    if (!activeSession.value && sessions.value.length) select(sessions.value[0]);
  } catch (e: any) { err.value = "会话加载失败：" + e.message; }
}
async function select(s: SessionInfo) {
  activeSession.value = s; activeId.value = s.id; loadingMsgs.value = true; messages.value = [];
  try { messages.value = (await sessionMessages(s.id, s.driver || "")).messages; err.value = ""; }
  catch (e: any) { err.value = "消息加载失败：" + e.message; }
  finally { loadingMsgs.value = false; }
}
async function loadDispatch() { dispatches.value = await dispatchRecent(30); }

async function refresh() {
  await Promise.all([loadSessions(), loadDispatch()]);
  if (activeSession.value) await select(activeSession.value);
}

onMounted(async () => {
  await Promise.all([loadSessions(), loadDispatch()]);
  timer = window.setInterval(() => { loadDispatch(); if (activeSession.value) select(activeSession.value); }, 6000);
});
onUnmounted(() => { if (timer) clearInterval(timer); });
</script>

<template>
  <div class="card">
    <h2>对话记录<button class="ghost small" style="margin-left:auto" @click="refresh">刷新</button></h2>
    <div v-if="err" class="msg err">{{ err }}</div>
    <div class="convo">
      <div class="sesslist">
        <div v-if="!sessions.length" class="empty">暂无会话。设备说话后这里会出现管家会话。</div>
        <div v-for="s in sessions" :key="s.id" class="sess" :class="{ on: s.id === activeId }" @click="select(s)">
          <div class="t">{{ s.title || s.cwdName || s.cwd || s.id }}</div>
          <div class="m">
            <span v-if="s.role">{{ s.role }}</span>
            <span v-if="s.cwdName">{{ s.cwdName }}</span>
            <span>{{ clock(s.lastUsedAt) }}</span>
          </div>
        </div>
      </div>

      <div class="stream">
        <div v-if="loadingMsgs" class="empty">加载中…</div>
        <div v-else-if="!messages.length" class="empty">该会话还没有消息。</div>
        <template v-for="(seg, si) in segments" :key="si">
          <div class="seg-div"><span>{{ seg.label }}</span></div>
          <div v-for="(m, i) in seg.items" :key="si + '-' + i" class="bub" :class="m.role">
            <div class="who">{{ m.role === 'user' ? '用户' : '管家' }}<span v-if="m.timestamp" class="ts">{{ clock(m.timestamp) }}</span></div>{{ m.content }}
          </div>
        </template>
      </div>
    </div>
  </div>

  <div class="card">
    <h2>最近派活任务</h2>
    <div v-if="!dispatches.length" class="empty">还没有派活记录。管家把编码任务派给 worker 后会出现在这里。</div>
    <div v-for="d in dispatches" :key="d.taskId" class="dcard">
      <div class="row">
        <span class="ti">{{ d.title || '(无标题任务)' }}</span>
        <span class="dstat" :class="d.status">{{ d.status }}</span>
      </div>
      <div class="row" style="margin-top:4px">
        <span>{{ d.cwd }}</span>
        <span>{{ d.elapsedMs ? (d.elapsedMs / 1000).toFixed(1) + 's' : clock(d.startedAt) }}</span>
      </div>
      <div v-if="d.error" class="msg err" style="margin-top:4px">{{ d.error }}</div>
    </div>
  </div>
</template>
