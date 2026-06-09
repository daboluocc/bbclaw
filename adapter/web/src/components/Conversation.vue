<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from "vue";
import { listSessions, sessionMessages, dispatchRecent, type SessionInfo, type ChatMessage, type DispatchEntry } from "../api";

// 两条消息间隔超过此值（30 分钟）时，分为新的会话段。
// 与固件端 BB_HISTORY_SEGMENT_GAP_MS 保持一致。
const GAP_MS = 30 * 60 * 1000;

const sessions = ref<SessionInfo[]>([]);
const activeId = ref<string | null>(null);
const activeSession = ref<SessionInfo | null>(null);
const messages = ref<ChatMessage[]>([]);
const dispatches = ref<DispatchEntry[]>([]);
const loadingMsgs = ref(false);
const pollingMsgs = ref(false);
const streamEl = ref<HTMLElement | null>(null);
const stickToBottom = ref(true);
const selectedContext = ref("");
const lastUpdated = ref("");
const err = ref("");
let timer: number | undefined;

type MessageBlock = ChatMessage & { parts: string[] };
type TurnBlock = {
  id: string;
  question?: MessageBlock;
  replies: MessageBlock[];
  startedAt?: string;
  endedAt?: string;
  contexts: string[];
};
type SegmentBlock = {
  id: string;
  label: string;
  range: string;
  turns: TurnBlock[];
  contexts: string[];
};

function clock(s?: string) {
  if (!s) return "";
  const d = new Date(s);
  return isNaN(+d) ? "" : d.toLocaleString();
}

function clockShort(s?: string) {
  if (!s) return "";
  const d = new Date(s);
  return isNaN(+d) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function mergeMessages(current: ChatMessage[], incoming: ChatMessage[]) {
  const byKey = new Map<string, ChatMessage>();
  for (const m of current) byKey.set(`${m.seq}:${m.role}`, m);
  for (const m of incoming) byKey.set(`${m.seq}:${m.role}`, m);
  return Array.from(byKey.values()).sort((a, b) => a.seq - b.seq);
}

function isNearBottom(el: HTMLElement) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

function rememberScrollIntent() {
  if (streamEl.value) stickToBottom.value = isNearBottom(streamEl.value);
}

async function settleScroll(shouldFollow: boolean) {
  await nextTick();
  if (shouldFollow && streamEl.value) streamEl.value.scrollTop = streamEl.value.scrollHeight;
}

function splitContent(content: string) {
  const trimmed = content.trimEnd();
  if (trimmed.length <= 1400) return [content];
  const paras = trimmed.split(/\n{2,}/);
  const chunks: string[] = [];
  let buf = "";
  for (const para of paras) {
    const next = buf ? `${buf}\n\n${para}` : para;
    if (next.length > 1400 && buf) {
      chunks.push(buf);
      buf = para;
    } else {
      buf = next;
    }
  }
  if (buf) chunks.push(buf);
  return chunks.flatMap((chunk) => {
    if (chunk.length <= 1800) return [chunk];
    const out: string[] = [];
    for (let i = 0; i < chunk.length; i += 1600) out.push(chunk.slice(i, i + 1600));
    return out;
  });
}

function urlsIn(text: string) {
  return Array.from(text.matchAll(/https?:\/\/[^\s)\]}>"'`]+/g)).map((m) => m[0]);
}

function pathsIn(text: string) {
  return Array.from(text.matchAll(/(?:~|\/(?:Users|home|tmp|var|opt|workspace|Volumes))\/[^\s)\]}>"'`]+/g)).map((m) => m[0]);
}

function normalizeContext(v: string) {
  return v.replace(/[.,;:，。；：]+$/, "");
}

function contextsForMessage(m: ChatMessage) {
  return [...urlsIn(m.content), ...pathsIn(m.content)].map(normalizeContext);
}

function contextsForSession(s?: SessionInfo | null) {
  return s?.cwd ? [s.cwd] : [];
}

function labelContext(v: string) {
  try {
    const u = new URL(v);
    return `${u.hostname}${u.pathname}`;
  } catch {
    const parts = v.split("/").filter(Boolean);
    return parts.length > 3 ? `.../${parts.slice(-3).join("/")}` : v;
  }
}

function messageMatchesContext(m: ChatMessage) {
  if (!selectedContext.value) return true;
  const target = selectedContext.value;
  return contextsForSession(activeSession.value).includes(target) || contextsForMessage(m).includes(target);
}

const visibleMessages = computed(() => messages.value.filter(messageMatchesContext));

const contextOptions = computed(() => {
  const counts = new Map<string, number>();
  for (const ctx of contextsForSession(activeSession.value)) counts.set(ctx, (counts.get(ctx) ?? 0) + messages.value.length);
  for (const m of messages.value) {
    for (const ctx of contextsForMessage(m)) counts.set(ctx, (counts.get(ctx) ?? 0) + 1);
  }
  return Array.from(counts, ([value, count]) => ({ value, label: labelContext(value), count }))
    .sort((a, b) => b.count - a.count || a.label.localeCompare(b.label))
    .slice(0, 10);
});

const messageCountLabel = computed(() => {
  const all = messages.value.length;
  const visible = visibleMessages.value.length;
  return selectedContext.value ? `${visible} / ${all} 条` : `${all} 条`;
});

function messageTime(m: ChatMessage) {
  return m.timestamp ? new Date(m.timestamp).getTime() : NaN;
}

function blockForMessage(m: ChatMessage): MessageBlock {
  return { ...m, parts: splitContent(m.content) };
}

function turnStart(turn: TurnBlock) {
  const first = turn.question ?? turn.replies[0];
  return first?.timestamp;
}

function turnEnd(turn: TurnBlock) {
  const last = turn.replies[turn.replies.length - 1] ?? turn.question;
  return last?.timestamp;
}

const turns = computed<TurnBlock[]>(() => {
  const out: TurnBlock[] = [];
  let current: TurnBlock | undefined;
  for (const m of visibleMessages.value) {
    const block = blockForMessage(m);
    const ctx = contextsForMessage(m);
    if (m.role === "user") {
      // New user message always starts a fresh turn.
      current = {
        id: `turn-${m.seq}-${m.role}`,
        question: block,
        replies: [],
        startedAt: block.timestamp,
        endedAt: block.timestamp,
        contexts: ctx,
      };
      out.push(current);
    } else if (!current) {
      // Orphan assistant message with no preceding user turn (e.g. async
      // dispatch result arriving before a user message in the session).
      // Create a questionless turn rather than dropping the message.
      current = {
        id: `turn-${m.seq}-${m.role}`,
        question: undefined,
        replies: [block],
        startedAt: block.timestamp,
        endedAt: block.timestamp,
        contexts: ctx,
      };
      out.push(current);
    } else {
      // Append to the current turn. This also handles async-dispatch late
      // replies: they arrive as assistant messages without a preceding user
      // message in the same turn, and we attach them to the last open turn
      // rather than creating a spurious new one.
      current.replies.push(block);
      current.endedAt = block.timestamp || current.endedAt;
      current.contexts = Array.from(new Set([...current.contexts, ...ctx]));
    }
  }
  for (const turn of out) {
    turn.startedAt = turnStart(turn);
    turn.endedAt = turnEnd(turn);
  }
  return out;
});

const segments = computed(() => {
  const runs: SegmentBlock[] = [];
  let prevEnd = NaN;
  for (const turn of turns.value) {
    const first = turn.question ?? turn.replies[0];
    const t = first ? messageTime(first) : NaN;
    const split = runs.length === 0 || (!isNaN(t) && !isNaN(prevEnd) && t - prevEnd > GAP_MS);
    if (split) {
      runs.push({
        id: `segment-${runs.length + 1}-${turn.id}`,
        label: `会话段 ${runs.length + 1}`,
        range: "",
        turns: [],
        contexts: [],
      });
    }
    const run = runs[runs.length - 1];
    run.turns.push(turn);
    run.contexts = Array.from(new Set([...run.contexts, ...turn.contexts]));
    const last = turn.replies[turn.replies.length - 1] ?? turn.question ?? first;
    prevEnd = last ? messageTime(last) : prevEnd;
  }
  for (const run of runs) {
    const first = run.turns[0]?.startedAt;
    const last = run.turns[run.turns.length - 1]?.endedAt;
    run.range = first && last && first !== last ? `${clockShort(first)} - ${clockShort(last)}` : clockShort(first);
  }
  return runs;
});

async function loadSessions() {
  try {
    const next = await listSessions();
    sessions.value = next;
    err.value = "";
    if (activeSession.value) {
      const updated = next.find((s) => s.id === activeSession.value?.id);
      if (updated) activeSession.value = updated;
    } else if (next.length) {
      await select(next[0], true);
    }
  } catch (e: any) { err.value = "会话加载失败：" + e.message; }
}
async function loadMessages(s: SessionInfo, reset = false) {
  const token = `${s.id}:${s.driver || ""}`;
  if (reset) {
    loadingMsgs.value = true;
    messages.value = [];
  } else {
    pollingMsgs.value = true;
  }
  const shouldFollow = reset || stickToBottom.value;
  try {
    const page = await sessionMessages(s.id, s.driver || "");
    if (`${activeSession.value?.id}:${activeSession.value?.driver || ""}` !== token) return;
    messages.value = reset ? page.messages : mergeMessages(messages.value, page.messages);
    lastUpdated.value = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    err.value = "";
    await settleScroll(shouldFollow);
  }
  catch (e: any) { err.value = "消息加载失败：" + e.message; }
  finally {
    loadingMsgs.value = false;
    pollingMsgs.value = false;
  }
}
async function select(s: SessionInfo, reset = true) {
  activeSession.value = s; activeId.value = s.id; selectedContext.value = "";
  await loadMessages(s, reset);
}
async function loadDispatch() { dispatches.value = await dispatchRecent(30); }

async function refresh() {
  await Promise.all([loadSessions(), loadDispatch()]);
  if (activeSession.value) await loadMessages(activeSession.value, false);
}

onMounted(async () => {
  await Promise.all([loadSessions(), loadDispatch()]);
  timer = window.setInterval(() => { loadDispatch(); loadSessions(); if (activeSession.value) loadMessages(activeSession.value, false); }, 3000);
});
onUnmounted(() => { if (timer) clearInterval(timer); });
</script>

<template>
  <div class="card">
    <h2>
      对话记录
      <span v-if="lastUpdated" class="live">最新 {{ lastUpdated }}<i v-if="pollingMsgs"></i></span>
      <button class="ghost small" style="margin-left:auto" @click="refresh">刷新</button>
    </h2>
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

      <div class="stream-wrap">
        <div class="stream-tools">
          <div>
            <strong>{{ segments.length }}</strong> 段 · <strong>{{ turns.length }}</strong> 轮 · {{ messageCountLabel }}
          </div>
          <label v-if="contextOptions.length">
            <span>Path / URL</span>
            <select v-model="selectedContext">
              <option value="">全部来源</option>
              <option v-for="c in contextOptions" :key="c.value" :value="c.value">{{ c.label }} ({{ c.count }})</option>
            </select>
          </label>
        </div>
        <div ref="streamEl" class="stream" @scroll="rememberScrollIntent">
          <div v-if="loadingMsgs" class="empty">加载中…</div>
          <div v-else-if="!messages.length" class="empty">该会话还没有消息。</div>
          <div v-else-if="!visibleMessages.length" class="empty">当前 path / URL 下没有消息。</div>
          <section v-for="seg in segments" :key="seg.id" class="conv-segment">
            <div class="seg-head">
              <div>
                <span class="seg-title">{{ seg.label }}</span>
                <span>{{ seg.turns.length }} 轮 · {{ seg.range || '未记录时间' }}</span>
              </div>
              <div v-if="seg.contexts.length" class="seg-source" :title="seg.contexts.join('\n')">{{ labelContext(seg.contexts[0]) }}</div>
            </div>

            <article v-for="turn in seg.turns" :key="turn.id" class="turn-card">
              <div v-if="turn.question" class="qa-row user-row">
                <div class="role-pill">问</div>
                <div class="msg-panel user-panel">
                  <div class="msg-meta"><span>用户</span><time v-if="turn.question.timestamp">{{ clock(turn.question.timestamp) }}</time></div>
                  <div v-for="(part, pi) in turn.question.parts" :key="pi" class="part" :class="{ split: turn.question.parts.length > 1 }">{{ part }}</div>
                </div>
              </div>

              <div v-for="reply in turn.replies" :key="reply.seq + '-' + reply.role" class="qa-row">
                <div class="role-pill assistant-pill">答</div>
                <div class="msg-panel">
                  <div class="msg-meta"><span>管家</span><time v-if="reply.timestamp">{{ clock(reply.timestamp) }}</time><em v-if="reply.parts.length > 1">{{ reply.parts.length }} 段</em></div>
                  <div v-for="(part, pi) in reply.parts" :key="pi" class="part" :class="{ split: reply.parts.length > 1 }">{{ part }}</div>
                </div>
              </div>
            </article>
          </section>
        </div>
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
