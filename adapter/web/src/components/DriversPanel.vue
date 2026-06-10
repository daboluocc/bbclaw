<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { drivers, setActiveDriver, setButlerDriver, type DriverRow } from "../api";

const rows = ref<DriverRow[]>([]);
const activeDriver = ref("");
const butlerDriver = ref("");
const note = ref("");
const noteErr = ref(false);
const busy = ref(false);

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  const d = await drivers();
  rows.value = d.drivers;
  activeDriver.value = d.active_driver;
  butlerDriver.value = d.butler_driver;
}

// Butler-capable AND installed drivers are the only valid 管家 choices.
const butlerChoices = computed(() => rows.value.filter((r) => r.butler_capable && r.installed !== false));

function installed(r: DriverRow): boolean { return r.installed !== false; }

async function pickActive(r: DriverRow) {
  if (busy.value || !installed(r) || r.name === activeDriver.value) return;
  busy.value = true;
  try { await setActiveDriver(r.name); setNote(`通用驱动已切到 ${r.name}，下一条消息生效`, false); await load(); }
  catch (e: any) { setNote("切换失败：" + e.message, true); }
  finally { busy.value = false; }
}

async function pickButler(r: DriverRow) {
  if (busy.value || r.name === butlerDriver.value) return;
  busy.value = true;
  try { await setButlerDriver(r.name); setNote(`管家驱动已切到 ${r.name}，下一轮对话生效（会重置管家对话连续性）`, false); await load(); }
  catch (e: any) { setNote("切换失败：" + e.message, true); }
  finally { busy.value = false; }
}

onMounted(load);
</script>

<template>
  <div class="card">
    <h2>通用驱动（playground / 非管家会话）</h2>
    <p class="hint">网页对话与 Agent Bus 会话默认用的 CLI。未安装的灰掉无法选；切换后下一条不指定驱动的消息立即生效。</p>
    <div class="files">
      <button
        v-for="r in rows" :key="r.name"
        class="chip"
        :class="{ active: r.name === activeDriver, missing: !installed(r) }"
        :disabled="busy || !installed(r)"
        @click="pickActive(r)"
      >
        {{ r.name }}
        <span v-if="!installed(r)" class="sz">未安装</span>
        <span v-else-if="r.name === activeDriver" class="sz">当前</span>
      </button>
      <span v-if="!rows.length" class="empty">没有已注册驱动。</span>
    </div>
  </div>

  <div class="card">
    <h2>设备管家驱动（butler）</h2>
    <p class="hint">真机语音/文本管家用的 CLI。只能选 butler-capable 且已安装的驱动（管家需要 persona 注入 + 派活能力，目前仅 claude-code）。切换会重置管家对话连续性。</p>
    <div class="files">
      <button
        v-for="r in butlerChoices" :key="r.name"
        class="chip"
        :class="{ active: r.name === butlerDriver }"
        :disabled="busy"
        @click="pickButler(r)"
      >
        {{ r.name }}
        <span v-if="r.name === butlerDriver" class="sz">当前</span>
      </button>
      <span v-if="!butlerChoices.length" class="empty">没有可用的 butler-capable 驱动（确认 claude CLI 已安装）。</span>
    </div>
  </div>

  <div class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</div>
</template>
