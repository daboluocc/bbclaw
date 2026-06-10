<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import SystemPanel from "./components/SystemPanel.vue";
import AiPanel from "./components/AiPanel.vue";
import FilesPanel from "./components/FilesPanel.vue";
import Conversation from "./components/Conversation.vue";
import { getSettings, restartAdapter } from "./api";

type Tab = "system" | "ai" | "convo" | "files";

const TABS: { id: Tab; label: string; path: string }[] = [
  { id: "system", label: "系统配置", path: "/admin/system" },
  { id: "ai", label: "AI 配置", path: "/admin/ai" },
  { id: "convo", label: "个人对话", path: "/admin/conversations" },
  { id: "files", label: "AI 数据文件", path: "/admin/files" },
];

function tabFromPath(path: string): Tab {
  const p = path.replace(/\/+$/, "");
  if (p.endsWith("/conversations")) return "convo";
  if (p.endsWith("/files")) return "files";
  if (p.endsWith("/ai")) return "ai";
  return "system"; // /admin, /admin/system, /admin/projects (legacy) → system
}

const tab = ref<Tab>(tabFromPath(window.location.pathname));
const restartRequired = ref(false);
const restarting = ref(false);

function setTab(next: Tab) {
  tab.value = next;
  const path = TABS.find((t) => t.id === next)!.path;
  if (window.location.pathname !== path) window.history.pushState({}, "", path);
  refreshRestartFlag();
}

function syncFromHistory() { tab.value = tabFromPath(window.location.pathname); }

async function refreshRestartFlag() {
  try { restartRequired.value = (await getSettings()).restart_required; }
  catch { /* settings store may be disabled; hide the banner */ }
}

async function doRestart() {
  if (restarting.value) return;
  restarting.value = true;
  try { await restartAdapter(); } catch { /* the socket drops as the process re-execs */ }
  // Poll /healthz until the new image is up, then hard-reload the page.
  const t0 = Date.now();
  const poll = async () => {
    try { if ((await fetch("/healthz")).ok) { window.location.reload(); return; } } catch { /* down */ }
    if (Date.now() - t0 < 30000) setTimeout(poll, 1000);
    else restarting.value = false;
  };
  setTimeout(poll, 1500);
}

onMounted(() => { window.addEventListener("popstate", syncFromHistory); refreshRestartFlag(); });
onUnmounted(() => window.removeEventListener("popstate", syncFromHistory));
</script>

<template>
  <header class="hdr">
    <span class="wordmark">BBCLAW</span>
    <span class="sub">Adapter · 本地管理 · localhost only</span>
    <nav class="tabs">
      <button v-for="t in TABS" :key="t.id" class="tab" :class="{ on: tab === t.id }" @click="setTab(t.id)">{{ t.label }}</button>
    </nav>
  </header>

  <main>
    <div v-if="restartRequired" class="rbanner">
      <span class="rt">配置已保存，<b>重启适配器后生效</b>。重启会中断进行中的会话。</span>
      <button class="small" :disabled="restarting" @click="doRestart">{{ restarting ? "重启中…" : "立即重启" }}</button>
    </div>

    <SystemPanel v-if="tab === 'system'" @saved="refreshRestartFlag" />
    <AiPanel v-else-if="tab === 'ai'" @saved="refreshRestartFlag" />
    <Conversation v-else-if="tab === 'convo'" />
    <FilesPanel v-else />
  </main>
</template>
