<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import SystemPanel from "./components/SystemPanel.vue";
import AgentPanel from "./components/AgentPanel.vue";
import VoicePanel from "./components/VoicePanel.vue";
import FilesPanel from "./components/FilesPanel.vue";
import Conversation from "./components/Conversation.vue";
import LogsPanel from "./components/LogsPanel.vue";
import { getSettings, restartAdapter, getAdapterVersion, type VersionState } from "./api";

type Tab = "convo" | "settings" | "logs" | "files";
// 「设置」二级分类（ADR-031/ADR-025 重整）：连接 / 智能体 / 语音。
type SettingsTab = "conn" | "agent" | "voice";
const SETTINGS_TABS: { id: SettingsTab; label: string }[] = [
  { id: "conn", label: "连接" },
  { id: "agent", label: "智能体" },
  { id: "voice", label: "语音" },
];

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

// The settings sub-tab is mirrored in the URL query (?s=conn|agent|voice) so a
// refresh / shared link reopens the same sub-page instead of resetting to 连接.
function settingsTabFromURL(): SettingsTab {
  const s = new URLSearchParams(window.location.search).get("s");
  return SETTINGS_TABS.some((t) => t.id === s) ? (s as SettingsTab) : "conn";
}
const settingsTab = ref<SettingsTab>(settingsTabFromURL());

// settingsURL builds the settings path carrying the current/next sub-tab.
function settingsURL(sub: SettingsTab): string { return `/admin/settings?s=${sub}`; }

function setTab(next: Tab) {
  tab.value = next;
  const path = next === "settings" ? settingsURL(settingsTab.value) : TABS.find((t) => t.id === next)!.path;
  if (window.location.pathname + window.location.search !== path) window.history.pushState({}, "", path);
  refreshRestartFlag();
}

function setSettingsTab(next: SettingsTab) {
  settingsTab.value = next;
  // replaceState (not push): sub-tab switches shouldn't pile up history entries,
  // but the URL still carries the param so a refresh stays put.
  window.history.replaceState({}, "", settingsURL(next));
}

function syncFromHistory() {
  tab.value = tabFromPath(window.location.pathname);
  if (tab.value === "settings") settingsTab.value = settingsTabFromURL();
}

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
      <nav class="subtabs">
        <button
          v-for="st in SETTINGS_TABS" :key="st.id"
          class="subtab" :class="{ on: settingsTab === st.id }"
          @click="setSettingsTab(st.id)"
        >{{ st.label }}</button>
      </nav>
      <SystemPanel v-if="settingsTab === 'conn'" @saved="refreshRestartFlag" />
      <AgentPanel v-else-if="settingsTab === 'agent'" @saved="refreshRestartFlag" />
      <VoicePanel v-else @saved="refreshRestartFlag" />
    </template>
    <LogsPanel v-else-if="tab === 'logs'" />
    <FilesPanel v-else />
  </main>
</template>

<style scoped>
/* Settings sub-navigation (连接 / 智能体 / 语音). A lighter, pill-row variant of
   the header tabs so the category split reads clearly without competing with the
   top-level nav. */
.subtabs{display:flex;gap:6px;margin:0 0 4px;flex-wrap:wrap}
.subtab{font:inherit;cursor:pointer;background:transparent;border:1px solid var(--ghost);
  color:var(--dim);border-radius:7px;padding:6px 16px;font-size:12px;letter-spacing:.04em}
.subtab:hover{color:var(--lit);border-color:var(--dim)}
.subtab.on{color:#04110f;background:var(--accent);border-color:var(--accent);font-weight:600}

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
