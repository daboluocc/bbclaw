<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from "vue";
import {
  listSessions, sessionParts, sessionMessages, dispatchRecent,
  type SessionInfo, type ConvTurn, type ConvPart, type DispatchEntry,
} from "../api";

// 两条消息间隔超过此值（30 分钟）时，分为新的会话段。
// 与固件端 BB_HISTORY_SEGMENT_GAP_MS 保持一致。
const GAP_MS = 30 * 60 * 1000;

const sessions = ref<SessionInfo[]>([]);
const activeId = ref<string | null>(null);
const activeSession = ref<SessionInfo | null>(null);
const turnsRaw = ref<ConvTurn[]>([]);
const dispatches = ref<DispatchEntry[]>([]);
const selectedTaskId = ref<string | null>(null);
const loadingMsgs = ref(false);
const pollingMsgs = ref(false);
const streamEl = ref<HTMLElement | null>(null);
const stickToBottom = ref(true);
const selectedContext = ref("");
const lastUpdated = ref("");
const err = ref("");
// Expanded fold state, keyed by an arbitrary string. Thinking blocks and
// dispatch cards both collapse by default (ADR-029 §2.1/§2.4).
const openFolds = ref<Set<string>>(new Set());
// Lazy-loaded child (worker) transcripts for dispatch drill-down (ADR-029 §2.3),
// keyed by childSessionId. childErr tracks per-child load failures.
const childCache = ref<Record<string, ConvTurn[]>>({});
const childErr = ref<Record<string, string>>({});
let timer: number | undefined;

type SegmentBlock = { id: string; label: string; range: string; turns: ConvTurn[]; contexts: string[] };

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

function isOpenK(key: string) { return openFolds.value.has(key); }
function toggleK(key: string) {
  const next = new Set(openFolds.value);
  next.has(key) ? next.delete(key) : next.add(key);
  openFolds.value = next;
}
function isOpen(turnSeq: number, pi: number) { return isOpenK(`${turnSeq}:${pi}`); }
function toggleFold(turnSeq: number, pi: number) { toggleK(`${turnSeq}:${pi}`); }

// Toggling a dispatch card open lazy-loads the worker transcript for drill-down
// (ADR-029 §2.3). One level deep: the child's own parts render flat.
async function toggleDispatch(turnSeq: number, pi: number, childSessionId?: string) {
  const key = `${turnSeq}:${pi}`;
  toggleK(key);
  if (isOpenK(key) && childSessionId && !childCache.value[childSessionId]) {
    try {
      const driver = activeSession.value?.driver || "";
      const page = await sessionParts(childSessionId, driver);
      childCache.value = { ...childCache.value, [childSessionId]: page.turns };
    } catch (e: any) {
      childErr.value = { ...childErr.value, [childSessionId]: e.message || "子线程加载失败" };
    }
  }
}

function splitContent(content: string) {
  const trimmed = content.trimEnd();
  if (trimmed.length <= 1400) return [content];
  const paras = trimmed.split(/\n{2,}/);
  const chunks: string[] = [];
  let buf = "";
  for (const para of paras) {
    const next = buf ? `${buf}\n\n${para}` : para;
    if (next.length > 1400 && buf) { chunks.push(buf); buf = para; }
    else { buf = next; }
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
function normalizeContext(v: string) { return v.replace(/[.,;:，。；：]+$/, ""); }

// turnText concatenates a turn's text + thinking parts for context extraction.
function turnText(t: ConvTurn) {
  return t.parts.filter((p) => p.kind === "text" || p.kind === "thinking").map((p) => p.text || "").join("\n");
}
function contextsForTurn(t: ConvTurn) {
  const txt = turnText(t);
  return [...urlsIn(txt), ...pathsIn(txt)].map(normalizeContext);
}
function contextsForSession(s?: SessionInfo | null) { return s?.cwd ? [s.cwd] : []; }

function labelContext(v: string) {
  try {
    const u = new URL(v);
    return `${u.hostname}${u.pathname}`;
  } catch {
    const parts = v.split("/").filter(Boolean);
    return parts.length > 3 ? `.../${parts.slice(-3).join("/")}` : v;
  }
}
function turnMatchesContext(t: ConvTurn) {
  if (!selectedContext.value) return true;
  const target = selectedContext.value;
  return contextsForSession(activeSession.value).includes(target) || contextsForTurn(t).includes(target);
}

const visibleTurns = computed(() => turnsRaw.value.filter(turnMatchesContext));

const contextOptions = computed(() => {
  const counts = new Map<string, number>();
  for (const ctx of contextsForSession(activeSession.value)) counts.set(ctx, (counts.get(ctx) ?? 0) + turnsRaw.value.length);
  for (const t of turnsRaw.value) {
    for (const ctx of contextsForTurn(t)) counts.set(ctx, (counts.get(ctx) ?? 0) + 1);
  }
  return Array.from(counts, ([value, count]) => ({ value, label: labelContext(value), count }))
    .sort((a, b) => b.count - a.count || a.label.localeCompare(b.label))
    .slice(0, 10);
});

const messageCountLabel = computed(() => {
  const all = turnsRaw.value.length;
  const visible = visibleTurns.value.length;
  return selectedContext.value ? `${visible} / ${all} 轮` : `${all} 轮`;
});

function turnTime(t: ConvTurn) { return t.timestamp ? new Date(t.timestamp).getTime() : NaN; }

// dispatchClass maps a status onto the existing .dstat color buckets.
function dispatchClass(status?: string) {
  if (status === "done") return "done";
  if (status === "error") return "error";
  return "running"; // started / running / async → 青
}
function elapsedMs(ms?: number) {
  if (!ms || ms <= 0) return "";
  const s = ms / 1000;
  return s >= 60 ? `${(s / 60).toFixed(1)}min` : `${s.toFixed(1)}s`;
}
// dispatchChip renders the trailing status chip text for an inline dispatch card.
function dispatchChip(p: ConvPart) {
  const st = p.dispatch?.status || "started";
  const e = elapsedMs(p.dispatch?.elapsedMs);
  if (st === "done") return e ? `✓ ${e}` : "✓ 完成";
  if (st === "error") return "✕ 失败";
  return e ? `派发中 · ${e}` : "派发中";
}

const segments = computed(() => {
  const runs: SegmentBlock[] = [];
  let prevEnd = NaN;
  for (const turn of visibleTurns.value) {
    const t = turnTime(turn);
    const split = runs.length === 0 || (!isNaN(t) && !isNaN(prevEnd) && t - prevEnd > GAP_MS);
    if (split) {
      runs.push({ id: `segment-${runs.length + 1}-${turn.seq}`, label: `会话段 ${runs.length + 1}`, range: "", turns: [], contexts: [] });
    }
    const run = runs[runs.length - 1];
    run.turns.push(turn);
    run.contexts = Array.from(new Set([...run.contexts, ...contextsForTurn(turn)]));
    if (!isNaN(t)) prevEnd = t;
  }
  for (const run of runs) {
    const first = run.turns[0]?.timestamp;
    const last = run.turns[run.turns.length - 1]?.timestamp;
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

// loadTurns prefers the structured /parts endpoint (thinking + dispatch inline);
// when the driver doesn't support it (PARTS_NOT_SUPPORTED), it falls back to the
// flat /messages endpoint and synthesizes text-only turns (ADR-029 §2.2).
async function loadTurns(s: SessionInfo, reset = false) {
  const token = `${s.id}:${s.driver || ""}`;
  if (reset) { loadingMsgs.value = true; turnsRaw.value = []; }
  else { pollingMsgs.value = true; }
  const shouldFollow = reset || stickToBottom.value;
  try {
    let next: ConvTurn[];
    try {
      next = (await sessionParts(s.id, s.driver || "")).turns;
    } catch {
      const mp = await sessionMessages(s.id, s.driver || "");
      next = mp.messages.map((m) => ({ role: m.role, seq: m.seq, timestamp: m.timestamp, parts: [{ kind: "text", text: m.content }] }));
    }
    if (`${activeSession.value?.id}:${activeSession.value?.driver || ""}` !== token) return;
    turnsRaw.value = next;
    lastUpdated.value = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    err.value = "";
    await settleScroll(shouldFollow);
  }
  catch (e: any) { err.value = "消息加载失败：" + e.message; }
  finally { loadingMsgs.value = false; pollingMsgs.value = false; }
}
async function select(s: SessionInfo, reset = true) {
  activeSession.value = s; activeId.value = s.id; selectedContext.value = "";
  await loadTurns(s, reset);
}
async function loadDispatch() {
  dispatches.value = await dispatchRecent(30);
  // 若当前选中的 task 已被 ring 淘汰，自动取消选中。
  if (selectedTaskId.value && !dispatches.value.some((d) => d.taskId === selectedTaskId.value)) {
    selectedTaskId.value = null;
  }
}

const selectedDispatch = computed(() =>
  selectedTaskId.value ? dispatches.value.find((d) => d.taskId === selectedTaskId.value) ?? null : null,
);

function selectDispatch(d: DispatchEntry) {
  selectedTaskId.value = selectedTaskId.value === d.taskId ? null : d.taskId;
}

function elapsedLabel(d: DispatchEntry) {
  if (d.elapsedMs && d.elapsedMs > 0) {
    const s = d.elapsedMs / 1000;
    return s >= 60 ? `${(s / 60).toFixed(1)}min` : `${s.toFixed(1)}s`;
  }
  return clock(d.startedAt);
}

async function refresh() {
  await Promise.all([loadSessions(), loadDispatch()]);
  if (activeSession.value) await loadTurns(activeSession.value, false);
}

onMounted(async () => {
  await Promise.all([loadSessions(), loadDispatch()]);
  timer = window.setInterval(() => { loadDispatch(); loadSessions(); if (activeSession.value) loadTurns(activeSession.value, false); }, 3000);
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
            <strong>{{ segments.length }}</strong> 段 · {{ messageCountLabel }}
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
          <div v-else-if="!turnsRaw.length" class="empty">该会话还没有消息。</div>
          <div v-else-if="!visibleTurns.length" class="empty">当前 path / URL 下没有消息。</div>
          <section v-for="seg in segments" :key="seg.id" class="conv-segment">
            <div class="seg-head">
              <div>
                <span class="seg-title">{{ seg.label }}</span>
                <span>{{ seg.turns.length }} 轮 · {{ seg.range || '未记录时间' }}</span>
              </div>
              <div v-if="seg.contexts.length" class="seg-source" :title="seg.contexts.join('\n')">{{ labelContext(seg.contexts[0]) }}</div>
            </div>

            <article v-for="turn in seg.turns" :key="turn.seq" class="turn-card">
              <div class="qa-row" :class="{ 'user-row': turn.role === 'user' }">
                <div class="role-pill" :class="{ 'assistant-pill': turn.role !== 'user' }">{{ turn.role === 'user' ? '问' : '答' }}</div>
                <div class="msg-panel" :class="{ 'user-panel': turn.role === 'user' }">
                  <div class="msg-meta"><span>{{ turn.role === 'user' ? '用户' : '管家' }}</span><time v-if="turn.timestamp">{{ clock(turn.timestamp) }}</time></div>

                  <template v-for="(part, pi) in turn.parts" :key="pi">
                    <!-- ① 思考折叠流 -->
                    <div v-if="part.kind === 'thinking'" class="think" :class="{ open: isOpen(turn.seq, pi) }">
                      <div class="think-head" @click="toggleFold(turn.seq, pi)">
                        <span class="dots">⠿</span>
                        <span>{{ isOpen(turn.seq, pi) ? '思考过程' : '思考过程 · 已折叠' }}</span>
                        <span class="chev">▸</span>
                      </div>
                      <div v-if="isOpen(turn.seq, pi)" class="think-body">{{ part.text }}</div>
                    </div>

                    <!-- ③ 内联派发卡片 + ④ 子线程下钻 -->
                    <div v-else-if="part.kind === 'dispatch'" class="idisp" :class="[dispatchClass(part.dispatch?.status), { open: isOpen(turn.seq, pi) }]">
                      <div class="idisp-bar" @click="toggleDispatch(turn.seq, pi, part.dispatch?.childSessionId)">
                        <span class="idisp-glyph">◇</span>
                        <span class="idisp-title">{{ part.dispatch?.title || '(无标题派发)' }}</span>
                        <span v-if="part.dispatch?.cwd" class="idisp-cwd">{{ part.dispatch?.cwd }}</span>
                        <span class="idisp-spacer"></span>
                        <span class="dstat" :class="dispatchClass(part.dispatch?.status)">{{ dispatchChip(part) }}</span>
                        <span class="chev">▸</span>
                      </div>
                      <div v-if="isOpen(turn.seq, pi)" class="idisp-body">
                        <div class="idisp-meta">taskId {{ part.dispatch?.taskId || '—' }}</div>
                        <div v-if="part.dispatch?.error" class="msg err">{{ part.dispatch?.error }}</div>

                        <!-- 子线程：worker 自己的 thinking + 回复 -->
                        <template v-if="part.dispatch?.childSessionId">
                          <div class="child-label">子 agent 子线程 · <b>{{ part.dispatch?.childSessionId }}</b></div>
                          <div v-if="childErr[part.dispatch.childSessionId]" class="msg err">{{ childErr[part.dispatch.childSessionId] }}</div>
                          <div v-else-if="!childCache[part.dispatch.childSessionId]" class="idisp-childhint">加载子线程…</div>
                          <div v-else-if="!childCache[part.dispatch.childSessionId].length" class="idisp-childhint">子线程暂无记录。</div>
                          <div v-else class="child-thread">
                            <template v-for="(ct, ti) in childCache[part.dispatch.childSessionId]" :key="ti">
                              <template v-for="(cp, cpi) in ct.parts" :key="cpi">
                                <div v-if="cp.kind === 'thinking'" class="think" :class="{ open: isOpenK('c:'+part.dispatch.childSessionId+':'+ti+':'+cpi) }">
                                  <div class="think-head" @click="toggleK('c:'+part.dispatch.childSessionId+':'+ti+':'+cpi)">
                                    <span class="dots">⠿</span><span>子 agent 思考</span><span class="chev">▸</span>
                                  </div>
                                  <div v-if="isOpenK('c:'+part.dispatch.childSessionId+':'+ti+':'+cpi)" class="think-body">{{ cp.text }}</div>
                                </div>
                                <div v-else-if="cp.kind === 'dispatch'" class="toolchip">◇ {{ cp.dispatch?.title || '子派发' }} · {{ cp.dispatch?.status || 'started' }}</div>
                                <div v-else-if="cp.kind === 'tool'" class="toolchip">⚙ {{ cp.tool }}</div>
                                <div v-else class="part child-part" :class="{ user: ct.role === 'user' }">{{ cp.text }}</div>
                              </template>
                            </template>
                          </div>
                        </template>
                      </div>
                    </div>

                    <!-- 工具调用 chip -->
                    <div v-else-if="part.kind === 'tool'" class="toolchip">⚙ {{ part.tool }}<span v-if="part.text" class="toolhint"> · {{ part.text }}</span></div>

                    <!-- 文本 -->
                    <template v-else>
                      <div v-for="(chunk, ci) in splitContent(part.text || '')" :key="ci" class="part" :class="{ split: (part.text || '').length > 1400 && ci > 0 }">{{ chunk }}</div>
                    </template>
                  </template>
                </div>
              </div>
            </article>
          </section>
        </div>
      </div>

      <aside class="dispatch-rail">
        <div class="rail-head">
          <span>派活任务</span>
          <em>{{ dispatches.length }}</em>
        </div>
        <div v-if="selectedDispatch" class="dispatch-detail">
          <div class="dd-row">
            <span class="dd-k">任务</span>
            <span class="dd-v">{{ selectedDispatch.title || '(无标题任务)' }}</span>
          </div>
          <div class="dd-row">
            <span class="dd-k">状态</span>
            <span class="dstat" :class="selectedDispatch.status">{{ selectedDispatch.status }}</span>
            <span v-if="selectedDispatch.elapsedMs" class="dd-elapsed">{{ elapsedLabel(selectedDispatch) }}</span>
          </div>
          <div class="dd-row">
            <span class="dd-k">起始</span>
            <span class="dd-v">{{ clock(selectedDispatch.startedAt) }}</span>
          </div>
          <div class="dd-row">
            <span class="dd-k">CWD</span>
            <span class="dd-v dd-mono" :title="selectedDispatch.cwd">{{ selectedDispatch.cwd || '—' }}</span>
          </div>
          <div class="dd-row">
            <span class="dd-k">TaskID</span>
            <span class="dd-v dd-mono" :title="selectedDispatch.taskId">{{ selectedDispatch.taskId }}</span>
          </div>
          <div v-if="selectedDispatch.error" class="msg err dd-err">{{ selectedDispatch.error }}</div>
          <button class="ghost small dd-close" @click="selectedTaskId = null">收起详情</button>
        </div>
        <div v-else class="dispatch-hint">点击下方任务可查看 cwd / taskId / 耗时等详情。</div>
        <div class="dispatch-list">
          <div v-if="!dispatches.length" class="empty">还没有派活记录。</div>
          <button
            v-for="d in dispatches"
            :key="d.taskId"
            class="dcard"
            :class="{ on: d.taskId === selectedTaskId }"
            type="button"
            @click="selectDispatch(d)"
          >
            <div class="row">
              <span class="ti">{{ d.title || '(无标题任务)' }}</span>
              <span class="dstat" :class="d.status">{{ d.status }}</span>
            </div>
            <div class="row dcard-meta">
              <span class="dcard-cwd" :title="d.cwd">{{ d.cwd || '—' }}</span>
              <span>{{ elapsedLabel(d) }}</span>
            </div>
          </button>
        </div>
      </aside>
    </div>
  </div>
</template>
