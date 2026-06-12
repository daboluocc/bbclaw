<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import SystemPanel from "./components/SystemPanel.vue";
import AiPanel from "./components/AiPanel.vue";
import FilesPanel from "./components/FilesPanel.vue";
import Conversation from "./components/Conversation.vue";
import LogsPanel from "./components/LogsPanel.vue";
import { getSettings, restartAdapter, getAdapterVersion, type VersionState } from "./api";

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
// Header version chip — populated on mount via getAdapterVersion (loopback,
// best-effort, 10s timeout server-side). When `update_available` is true the
// chip lights accent + a red dot, mirroring clawflow web's dashboard banner
// pattern. Click jumps to 设置 → 设备身份 where the upgrade button lives.
const version = ref<VersionState>({ current: "", latest: "", update_available: false, updating: false });
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

onMounted(() => {
  window.addEventListener("popstate", syncFromHistory);
  refreshRestartFlag();
  // Best-effort version probe: failure stays silent (chip just shows "—").
  getAdapterVersion().then((v) => { version.value = v; });
});
onUnmounted(() => window.removeEventListener("popstate", syncFromHistory));
</script>

<template>
  <header class="hdr">
    <span class="wordmark">BBCLAW</span>
    <button
      v-if="version.current"
      class="verchip"
      :class="{ due: version.update_available }"
      :title="version.update_available
        ? `当前 ${version.current} → 有新版本 ${version.latest}，点击前往升级`
        : version.latest
          ? `当前 ${version.current}，已是最新`
          : `当前 ${version.current}`"
      @click="setTab('settings')"
    >
      <span class="vlabel">{{ version.current }}</span>
      <span v-if="version.update_available" class="vdot" />
    </button>
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
/* Version chip pinned to header left — sits just right of the BBCLAW wordmark.
   Subtle by default (matches the 重启 button's idle look); when an upgrade is
   available the chip switches to accent border + a pulsing red dot so it reads
   like a notification badge. Clicking it jumps to 设置 where the upgrade
   button lives, mirroring clawflow web's dashboard upgrade banner. */
.verchip{align-self:center;display:inline-flex;align-items:center;gap:6px;cursor:pointer;
  background:transparent;border:1px solid var(--ghost);color:var(--dim);border-radius:7px;
  padding:4px 9px;font:inherit;font-size:11px;letter-spacing:.04em;white-space:nowrap;
  font-variant-numeric:tabular-nums}
.verchip:hover{color:var(--lit);border-color:var(--dim)}
.verchip.due{color:var(--accent);border-color:var(--accent)}
.verchip.due:hover{filter:brightness(1.15)}
.verchip .vlabel{max-width:160px;overflow:hidden;text-overflow:ellipsis}
.verchip .vdot{width:7px;height:7px;border-radius:50%;background:#ff5566;
  box-shadow:0 0 0 0 rgba(255,85,102,.55);animation:vpulse 1.4s ease-in-out infinite}
@keyframes vpulse{
  0%,100%{box-shadow:0 0 0 0 rgba(255,85,102,.55)}
  50%{box-shadow:0 0 0 4px rgba(255,85,102,0)}
}
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
