<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import StatusBar from "./StatusBar.vue";
import { getSettings, getAdapterVersion, triggerAdapterUpdate, type VersionState } from "../api";

// 「设备 / 关于」：只读身份 + 维护动作（检查 / 一键升级）。不涉及任何运行配置，
// 所以没有保存按钮——纯参考 + 升级。
const derived = reactive({ home_site_id: "", version: "" });
const version = ref<VersionState>({ current: "", latest: "", update_available: false, updating: false });
const checking = ref(false);
const upgrading = ref(false);
const note = ref("");
const noteErr = ref(false);

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const state = await getSettings();
    derived.home_site_id = state.derived?.home_site_id ?? "";
    derived.version = state.derived?.version ?? "";
  } catch { /* settings store may be disabled; identity card just hides */ }
}

async function checkVersion(verbose = false) {
  if (checking.value) return;
  checking.value = true;
  if (verbose) setNote("正在检查更新...", false);
  try {
    version.value = await getAdapterVersion();
    if (verbose) {
      if (!version.value.latest) setNote("无法获取最新版本（网络问题或 GitHub 限流）", true);
      else if (version.value.update_available) setNote(`发现新版本 ${version.value.latest}`, false);
      else setNote("已是最新版本", false);
    }
  } catch (e: any) {
    if (verbose) setNote("检查失败：" + e.message, true);
  } finally {
    checking.value = false;
  }
}

async function doUpgrade() {
  if (upgrading.value) return;
  if (!confirm(`即将升级到 ${version.value.latest}，过程中适配器会自动重启（中断进行中的会话）。继续？`)) return;
  upgrading.value = true;
  setNote("正在下载并替换二进制...", false);
  try {
    const res = await triggerAdapterUpdate();
    if (!res.upgraded) { setNote("已是最新版本", false); await checkVersion(); return; }
    if (!res.restarting) { setNote("升级完成，请手动重启适配器以生效", false); return; }
    setNote("升级完成，等待重启...", false);
    const t0 = Date.now();
    const poll = async () => {
      try { if ((await fetch("/healthz")).ok) { window.location.reload(); return; } } catch { /* down */ }
      if (Date.now() - t0 < 60000) setTimeout(poll, 1000);
      else { setNote("升级完成但重启超时，请手动刷新页面", true); upgrading.value = false; }
    };
    setTimeout(poll, 1500);
  } catch (e: any) {
    setNote("升级失败：" + e.message, true);
    upgrading.value = false;
  }
}

onMounted(() => { load(); checkVersion(false); });
</script>

<template>
  <StatusBar />

  <div class="card" v-if="derived.home_site_id || derived.version">
    <h2>设备身份</h2>
    <p class="hint">系统自动生成，只读。配对 / 排障时引用即可，无需手动设置。</p>
    <div class="status-grid">
      <div class="item" v-if="derived.home_site_id">
        <div class="k">Home Site ID</div><div class="v" style="word-break:break-all">{{ derived.home_site_id }}</div>
      </div>
      <div class="item" v-if="derived.version">
        <div class="k">适配器版本</div>
        <div class="v">
          <span>{{ derived.version }}</span>
          <span v-if="version.update_available" class="ver-badge new" :title="`最新版本 ${version.latest}`">有新版本 {{ version.latest }}</span>
          <span v-else-if="version.latest && !version.update_available" class="ver-badge ok" title="已是最新">已是最新</span>
        </div>
      </div>
    </div>
    <div class="upgrade-row">
      <button class="ghost" :disabled="checking || upgrading" @click="checkVersion(true)">
        {{ checking ? "检查中..." : "检查更新" }}
      </button>
      <button v-if="version.update_available" :disabled="upgrading || checking" @click="doUpgrade">
        {{ upgrading ? "升级中..." : `一键升级到 ${version.latest}` }}
      </button>
      <span v-if="note" class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>

<style scoped>
.status-grid .v .ver-badge{margin-left:8px;font-size:10px;letter-spacing:.05em;
  border:1px solid var(--ghost);border-radius:99px;padding:2px 8px;color:var(--dim);white-space:nowrap}
.status-grid .v .ver-badge.new{color:#04110f;background:var(--accent);border-color:var(--accent);font-weight:600}
.status-grid .v .ver-badge.ok{color:var(--dim)}
.upgrade-row{display:flex;align-items:center;gap:11px;margin-top:14px;flex-wrap:wrap}
.upgrade-row .msg{margin-top:0}
</style>
