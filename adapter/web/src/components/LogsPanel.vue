<script setup lang="ts">
import { nextTick, onMounted, onUnmounted, ref } from "vue";
import { adapterLogs } from "../api";

const lines = ref<string[]>([]);
const file = ref("");
const lastUpdated = ref("");
const follow = ref(true);
const paused = ref(false);
const streamEl = ref<HTMLElement | null>(null);
let timer: number | undefined;

function isNearBottom(el: HTMLElement) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < 60;
}
function onScroll() {
  if (streamEl.value) follow.value = isNearBottom(streamEl.value);
}

async function load() {
  if (paused.value) return;
  const shouldFollow = follow.value;
  const d = await adapterLogs(800);
  lines.value = d.lines;
  if (d.file) file.value = d.file;
  lastUpdated.value = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  if (shouldFollow) {
    await nextTick();
    if (streamEl.value) streamEl.value.scrollTop = streamEl.value.scrollHeight;
  }
}

function levelOf(l: string): string {
  const m = l.match(/level=(\w+)/);
  return m ? m[1].toLowerCase() : "";
}

async function copyPath() {
  try { await navigator.clipboard.writeText(file.value); } catch { /* clipboard may be blocked */ }
}

onMounted(() => { load(); timer = window.setInterval(load, 3000); });
onUnmounted(() => { if (timer) clearInterval(timer); });
</script>

<template>
  <div class="card">
    <h2>
      运行日志
      <span v-if="lastUpdated" class="live">最新 {{ lastUpdated }}</span>
      <button class="ghost small" style="margin-left:auto" @click="paused = !paused">{{ paused ? "继续" : "暂停" }}</button>
      <button class="ghost small" @click="load">刷新</button>
    </h2>
    <p class="hint">
      适配器的运行输出（内存中最近若干行）。无需再盯着二进制的启动终端。
    </p>
    <div v-if="file" class="logfile">
      <span class="k">日志文件</span>
      <code class="p">{{ file }}</code>
      <button class="ghost small" @click="copyPath">复制路径</button>
    </div>

    <div ref="streamEl" class="logstream" @scroll="onScroll">
      <div v-if="!lines.length" class="empty">暂无日志输出。</div>
      <div v-for="(l, i) in lines" :key="i" class="logline" :class="levelOf(l)">{{ l }}</div>
    </div>
    <p v-if="!follow" class="hint" style="margin:8px 0 0">已暂停跟随（向上滚动了）。滚到底部恢复自动跟随。</p>
  </div>
</template>

<style scoped>
.logfile{display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin:4px 0 12px}
.logfile .k{font-size:10px;text-transform:uppercase;letter-spacing:.1em;color:var(--dim)}
.logfile .p{font-size:12px;color:var(--lit);word-break:break-all;background:rgba(7,11,14,.6);
  border:1px solid var(--ghost);border-radius:6px;padding:3px 7px}
.logstream{height:62vh;min-height:320px;overflow:auto;background:rgba(7,11,14,.6);
  border:1px solid var(--ghost);border-radius:8px;padding:10px 12px}
.logline{font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  white-space:pre-wrap;word-break:break-word;color:var(--dim)}
.logline.info{color:var(--lit)}
.logline.warn{color:#e7c14c}
.logline.error{color:var(--err)}
</style>
