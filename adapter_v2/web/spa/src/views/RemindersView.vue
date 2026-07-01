<script setup lang="ts">
import { ref, onMounted, onUnmounted } from "vue";
import { api, type Reminder } from "../api";

const reminders = ref<Reminder[]>([]);
const loading = ref(false);
const listError = ref("");

// Create form.
const prompt = ref("");
const minutes = ref<number | null>(30);
const mode = ref<"notify" | "task">("notify");
const creating = ref(false);

const toast = ref<{ text: string; err: boolean } | null>(null);
let toastTimer: number | null = null;
function showToast(text: string, err = false) {
  toast.value = { text, err };
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => (toast.value = null), 2600);
}

const POLL_MS = 4000;
let pollTimer: number | null = null;

function fmtTime(epoch: number): string {
  const d = new Date(epoch * 1000);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleString([], {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// A short relative "in 25m" / "12m ago" for the next-fire column.
function relTime(epoch: number): string {
  const secs = epoch - Date.now() / 1000;
  const abs = Math.abs(secs);
  const mins = Math.round(abs / 60);
  if (abs < 60) return secs >= 0 ? "即将" : "刚刚";
  if (mins < 60) return secs >= 0 ? `${mins} 分钟后` : `${mins} 分钟前`;
  const hrs = Math.round(mins / 6) / 10;
  if (abs < 86400) return secs >= 0 ? `${hrs} 小时后` : `${hrs} 小时前`;
  const days = Math.round(abs / 86400);
  return secs >= 0 ? `${days} 天后` : `${days} 天前`;
}

const stateLabel: Record<string, string> = {
  scheduled: "计划中",
  running: "运行中",
  done: "已完成",
  failed: "失败",
  canceled: "已取消",
};

async function load(silent = false) {
  if (!silent) loading.value = true;
  listError.value = "";
  try {
    const data = await api.listReminders();
    reminders.value = data.reminders || [];
  } catch (e) {
    listError.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}

async function create() {
  if (!prompt.value.trim()) {
    showToast("请填写提醒内容", true);
    return;
  }
  if (!minutes.value || minutes.value <= 0) {
    showToast("请填写多少分钟后提醒", true);
    return;
  }
  creating.value = true;
  try {
    await api.createReminder({
      prompt: prompt.value.trim(),
      delay: `${Math.round(minutes.value)}m`,
      mode: mode.value,
    });
    prompt.value = "";
    showToast("已设置提醒");
    await load(true);
  } catch (e) {
    showToast((e as Error).message, true);
  } finally {
    creating.value = false;
  }
}

async function cancel(r: Reminder) {
  try {
    await api.cancelReminder(r.id);
    showToast("已取消");
    await load(true);
  } catch (e) {
    showToast((e as Error).message, true);
  }
}

onMounted(() => {
  load();
  pollTimer = window.setInterval(() => load(true), POLL_MS);
});
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer);
  if (toastTimer) clearTimeout(toastTimer);
});
</script>

<template>
  <section class="reminders">
    <!-- create -->
    <form class="new" @submit.prevent="create">
      <input
        v-model="prompt"
        class="in prompt"
        placeholder="提醒我… 例如「检查烧录日志有没有报错并告诉我」"
        :disabled="creating"
      />
      <label class="fld">
        <input
          v-model.number="minutes"
          class="in mins"
          type="number"
          min="1"
          :disabled="creating"
        />
        <span class="unit">分钟后</span>
      </label>
      <select v-model="mode" class="in mode" :disabled="creating">
        <option value="notify">提醒(念一句)</option>
        <option value="task">跑任务(汇报结果)</option>
      </select>
      <button class="btn go" type="submit" :disabled="creating">
        {{ creating ? "设置中…" : "＋ 新建" }}
      </button>
    </form>
    <p class="hint">
      「提醒」到点念出提醒词；「跑任务」到点由助手执行并把结果念给你听。
    </p>

    <!-- list -->
    <div v-if="listError" class="err-line">加载失败：{{ listError }}</div>
    <div v-else-if="loading && !reminders.length" class="muted">加载中…</div>
    <div v-else-if="!reminders.length" class="muted">还没有提醒。</div>
    <ul v-else class="list">
      <li v-for="r in reminders" :key="r.id" class="row" :class="r.state">
        <span class="mode-chip" :class="r.mode">{{
          r.mode === "task" ? "任务" : "提醒"
        }}</span>
        <span class="body">
          <span class="prompt-txt">{{ r.prompt }}</span>
          <span class="meta">
            {{ fmtTime(r.runAt) }} · {{ relTime(r.runAt) }}
          </span>
        </span>
        <span class="state" :class="r.state">{{
          stateLabel[r.state] || r.state
        }}</span>
        <button
          v-if="r.state === 'scheduled'"
          class="btn cancel"
          @click="cancel(r)"
        >
          取消
        </button>
        <span v-else class="cancel-gap"></span>
      </li>
    </ul>

    <div v-if="toast" class="toast" :class="{ err: toast.err }">
      {{ toast.text }}
    </div>
  </section>
</template>

<style scoped>
.reminders {
  max-width: 760px;
  margin: 0 auto;
  padding: 16px 14px 40px;
}
.new {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
}
.in {
  background: var(--bg);
  border: 1px solid var(--ghost);
  color: var(--lit);
  padding: 8px 10px;
  font: inherit;
  border-radius: 2px;
}
.in:focus {
  outline: none;
  border-color: var(--accent);
}
.prompt {
  flex: 1 1 320px;
}
.fld {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--dim);
}
.mins {
  width: 68px;
}
.mode {
  color: var(--lit);
}
.btn {
  background: transparent;
  border: 1px solid var(--ghost);
  color: var(--dim);
  padding: 8px 12px;
  cursor: pointer;
  border-radius: 2px;
  font: inherit;
}
.btn:hover {
  border-color: var(--accent);
  color: var(--accent);
}
.btn.go {
  border-color: var(--accent);
  color: var(--accent);
}
.btn:disabled {
  opacity: 0.5;
  cursor: default;
}
.hint {
  color: var(--dim);
  font-size: 12px;
  margin: 8px 2px 18px;
}
.list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px;
  border: 1px solid var(--ghost);
  border-radius: 2px;
}
.row.canceled,
.row.done {
  opacity: 0.55;
}
.mode-chip {
  flex: 0 0 auto;
  font-size: 11px;
  padding: 2px 7px;
  border-radius: 2px;
  border: 1px solid var(--ghost);
  color: var(--dim);
}
.mode-chip.task {
  border-color: var(--accent);
  color: var(--accent);
}
.body {
  flex: 1 1 auto;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.prompt-txt {
  color: var(--lit);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.meta {
  color: var(--dim);
  font-size: 12px;
}
.state {
  flex: 0 0 auto;
  font-size: 12px;
  color: var(--dim);
}
.state.done {
  color: var(--ok);
}
.state.failed {
  color: var(--err);
}
.state.scheduled {
  color: var(--accent);
}
.cancel {
  flex: 0 0 auto;
  padding: 4px 10px;
  font-size: 12px;
}
.cancel-gap {
  flex: 0 0 auto;
  width: 52px;
}
.err-line {
  color: var(--err);
}
.muted {
  color: var(--dim);
  padding: 20px 2px;
}
.toast {
  position: fixed;
  left: 50%;
  bottom: 24px;
  transform: translateX(-50%);
  background: var(--ghost);
  color: var(--lit);
  border: 1px solid var(--accent);
  padding: 8px 16px;
  border-radius: 2px;
  font-size: 13px;
}
.toast.err {
  border-color: var(--err);
  color: var(--err);
}
</style>
