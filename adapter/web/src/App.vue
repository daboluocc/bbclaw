<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import SystemPanel from "./components/SystemPanel.vue";
import AiPanel from "./components/AiPanel.vue";
import FilesPanel from "./components/FilesPanel.vue";
import Conversation from "./components/Conversation.vue";
import LogsPanel from "./components/LogsPanel.vue";
import { getSettings, restartAdapter } from "./api";

type Tab = "convo" | "settings" | "logs" | "files";

const TABS: { id: Tab; label: string; path: string }[] = [
  { id: "convo", label: "个人对话", path: "/admin/conversations" },
  { id: "settings", label: "设置", path: "/admin/settings" },
  { id: "logs", label: "日志", path: "/admin/logs" },
  { id: "files", label: "AI 数据文件", path: "/admin/files" },
];

function tabFromPath(path: string): Tab {
  const p = path.replace(/\/+$/, "");
  if (p.endsWith("/files")) return "files";
  if (p.endsWith("/logs")) return "logs";
  // 「设置」合并了原系统配置 / AI 配置；老书签 /system /ai /projects 仍落到这里。
  if (p.endsWith("/settings") || p.endsWith("/system") || p.endsWith("/ai") || p.endsWith("/projects")) return "settings";
  return "convo"; // /admin, /admin/conversations → 默认聊天页
}

const tab = ref<Tab>(tabFromPath(window.location.pathname));
const restartRequired = ref(false);
const restarting = ref(false);
// 部署模式（系统配置）保存后 bump 这个 key，重挂 AI 面板，让它重新读取 cloud/local
// 并据此刷新下方 ASR/TTS 区是否显示。
const aiReloadKey = ref(0);

function setTab(next: Tab) {
  tab.value = next;
  const path = TABS.find((t) => t.id === next)!.path;
  if (window.location.pathname !== path) window.history.pushState({}, "", path);
  refreshRestartFlag();
}

function syncFromHistory() { tab.value = tabFromPath(window.location.pathname); }

function onSystemSaved() { aiReloadKey.value++; refreshRestartFlag(); }

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
    <button
      class="restartctl"
      :class="{ due: restartRequired }"
      :disabled="restarting"
      :title="restartRequired ? '配置已保存，重启后生效（会中断进行中的会话）' : '重启适配器（会中断进行中的会话）'"
      @click="doRestart"
    >
      <span class="dot" />{{ restarting ? "重启中…" : restartRequired ? "重启生效" : "重启" }}
    </button>
  </header>

  <main>
    <Conversation v-if="tab === 'convo'" />
    <template v-else-if="tab === 'settings'">
      <SystemPanel @saved="onSystemSaved" />
      <AiPanel :key="aiReloadKey" @saved="refreshRestartFlag" />
    </template>
    <LogsPanel v-else-if="tab === 'logs'" />
    <FilesPanel v-else />
  </main>
</template>

<style scoped>
/* Restart control, pinned top-right of the header. Subtle by default; lights up
   (accent + pulsing dot) when a saved settings change is pending a restart. */
.restartctl{align-self:center;display:inline-flex;align-items:center;gap:7px;font:inherit;cursor:pointer;
  background:transparent;border:1px solid var(--ghost);color:var(--dim);border-radius:7px;
  padding:6px 12px;font-size:12px;letter-spacing:.05em;white-space:nowrap}
.restartctl:hover:not(:disabled){color:var(--lit);border-color:var(--dim)}
.restartctl:disabled{opacity:.6;cursor:default}
.restartctl .dot{width:7px;height:7px;border-radius:50%;background:var(--ghost)}
.restartctl.due{color:#04110f;background:var(--accent);border-color:var(--accent);font-weight:600}
.restartctl.due .dot{background:#04110f;animation:rpulse 1.2s ease-in-out infinite}
@keyframes rpulse{0%,100%{opacity:1}50%{opacity:.25}}
</style>
