<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import StatusBar from "./StatusBar.vue";
import { getSettings, putSettings, getAdapterVersion, triggerAdapterUpdate, type VersionState } from "../api";

const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const advanced = ref(false);

// One choice drives the page: cloud (cloud does ASR/TTS, local needs nothing) or
// local (device LAN-direct, you configure ASR/TTS below).
const mode = ref<"cloud" | "local">("cloud");
// cloud.home_site_id is kept in state only so a save round-trips the stored
// value (usually empty → auto-derived); it is never shown as an editable field.
// The OpenClaw gateway is a driver-level concern (it shows up under 驱动 like
// claude/codex), so it's no longer a config section here.
const cloud = reactive({ ws_url: "", auth_token: "", home_site_id: "" });
const audio = reactive({ save_audio: false, save_input_on_finish: true });
// Read-only, system-generated identity surfaced for reference (not editable).
const derived = reactive({ home_site_id: "", version: "" });

// Self-upgrade UI state (ADR-025 follow-up). version mirrors GET
// /v1/admin/version; checking/upgrading drive the spinner + button states;
// upgradeNote is a transient toast for "已是最新" / failure messages so the user
// always gets feedback even when nothing visibly changed.
const version = ref<VersionState>({ current: "", latest: "", update_available: false, updating: false });
const checking = ref(false);
const upgrading = ref(false);
const upgradeNote = ref("");
const upgradeNoteErr = ref(false);

function setUpgradeNote(t: string, err: boolean) {
  upgradeNote.value = t;
  upgradeNoteErr.value = err;
}

async function checkVersion(verbose = false) {
  if (checking.value) return;
  checking.value = true;
  if (verbose) setUpgradeNote("正在检查更新...", false);
  try {
    version.value = await getAdapterVersion();
    if (verbose) {
      if (!version.value.latest) setUpgradeNote("无法获取最新版本（网络问题或 GitHub 限流）", true);
      else if (version.value.update_available) setUpgradeNote(`发现新版本 ${version.value.latest}`, false);
      else setUpgradeNote("已是最新版本", false);
    }
  } catch (e: any) {
    if (verbose) setUpgradeNote("检查失败：" + e.message, true);
  } finally {
    checking.value = false;
  }
}

async function doUpgrade() {
  if (upgrading.value) return;
  if (!confirm(`即将升级到 ${version.value.latest}，过程中适配器会自动重启（中断进行中的会话）。继续？`)) return;
  upgrading.value = true;
  setUpgradeNote("正在下载并替换二进制...", false);
  try {
    const res = await triggerAdapterUpdate();
    if (!res.upgraded) {
      setUpgradeNote("已是最新版本", false);
      await checkVersion();
      return;
    }
    if (!res.restarting) {
      setUpgradeNote("升级完成，请手动重启适配器以生效", false);
      return;
    }
    setUpgradeNote("升级完成，等待重启...", false);
    // Same poll-and-reload recipe as the 重启 button: hit /healthz until the
    // re-execed process answers, then hard-reload the SPA so it picks up any
    // new admin assets shipped with the new binary.
    const t0 = Date.now();
    const poll = async () => {
      try { if ((await fetch("/healthz")).ok) { window.location.reload(); return; } } catch { /* down */ }
      if (Date.now() - t0 < 60000) setTimeout(poll, 1000);
      else { setUpgradeNote("升级完成但重启超时，请手动刷新页面", true); upgrading.value = false; }
    };
    setTimeout(poll, 1500);
  } catch (e: any) {
    setUpgradeNote("升级失败：" + e.message, true);
    upgrading.value = false;
  }
}

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const state = await getSettings();
    const { settings } = state;
    Object.assign(cloud, settings.cloud);
    audio.save_audio = settings.voice.save_audio;
    audio.save_input_on_finish = settings.voice.save_input_on_finish;
    mode.value = settings.topology.local_voice_enabled ? "local" : "cloud";
    derived.home_site_id = state.derived?.home_site_id ?? "";
    derived.version = state.derived?.version ?? "";
    loaded.value = true;
  } catch (e: any) { setNote("加载失败：" + e.message, true); }
}

async function save() {
  if (busy.value) return;
  busy.value = true;
  try {
    const topology = mode.value === "local"
      ? { cloud_relay_enabled: false, local_voice_enabled: true }
      : { cloud_relay_enabled: true, local_voice_enabled: false };
    const res = await putSettings({
      topology,
      cloud: { ...cloud },
      voice: { save_audio: audio.save_audio, save_input_on_finish: audio.save_input_on_finish },
    });
    if (mode.value === "local" && res.voice_incomplete)
      setNote("已保存。但 ASR/TTS 还没填完整 → 在本页下方「语音（ASR / TTS）」填好后语音才可用。", true);
    else
      setNote("已保存。重启适配器后生效。", false);
    emit("saved");
  } catch (e: any) { setNote("保存失败：" + e.message, true); }
  finally { busy.value = false; }
}

onMounted(() => {
  load();
  // Silent best-effort check — failure is invisible so a flaky network never
  // shows an alarming red note on first paint. The user can hit 「检查更新」
  // for a verbose retry if they suspect something's off.
  checkVersion(false);
});
</script>

<template>
  <StatusBar />

  <div class="card">
    <h2>部署模式</h2>
    <p class="hint">选一种即可——页面只显示该模式需要配置的项。</p>
    <div class="modes">
      <label class="mode" :class="{ on: mode === 'cloud' }">
        <input type="radio" value="cloud" v-model="mode" />
        <div>
          <div class="mt">☁ 云端模式（推荐）</div>
          <div class="md">设备经云端，语音（ASR/TTS）由云端处理。本地无需任何配置，开箱即用。</div>
        </div>
      </label>
      <label class="mode" :class="{ on: mode === 'local' }">
        <input type="radio" value="local" v-model="mode" />
        <div>
          <div class="mt">⌂ 本地模式</div>
          <div class="md">设备 LAN 直连本机，语音在本机处理。需要在本页下方填写 ASR / TTS 密钥。</div>
        </div>
      </label>
    </div>

    <p v-if="mode === 'cloud'" class="hint ok-hint">
      ✓ 无需本地配置。在网页端 daboluo.cc 输入配对码即可激活设备。
    </p>
    <p v-else class="hint">下一步 → 在本页下方填写 ASR / TTS，否则语音不可用。</p>
  </div>

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
          <span v-if="version.update_available" class="ver-badge new" :title="`最新版本 ${version.latest}`">
            有新版本 {{ version.latest }}
          </span>
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
      <span v-if="upgradeNote" class="msg" :class="{ err: upgradeNoteErr, ok: !upgradeNoteErr }">{{ upgradeNote }}</span>
    </div>
  </div>

  <div class="card">
    <button class="disclose" @click="advanced = !advanced">
      {{ advanced ? "▾" : "▸" }} 高级设置<span class="hint" style="margin:0 0 0 8px">（一般不用动）</span>
    </button>

    <div v-if="advanced">
      <div class="subsec" v-if="mode === 'cloud'">
        <div class="lbl">自建云 relay</div>
        <p class="hint" style="margin:0 0 10px">默认指向生产云，开箱即用；只有自建云端时才需要改。</p>
        <div class="form">
          <div class="field"><label>云端 WS 地址</label>
            <input type="text" v-model="cloud.ws_url" placeholder="wss://bbclaw.daboluo.cc/ws" /></div>
          <div class="field"><label>Auth Token</label>
            <input type="text" v-model="cloud.auth_token" placeholder="云端关闭匿名接入时才需要" /></div>
        </div>
      </div>

      <div class="subsec" v-if="mode === 'local'">
        <div class="lbl">音频留存</div>
        <label class="toggle">
          <input type="checkbox" v-model="audio.save_input_on_finish" />
          <div><div class="tl">保存输入音频</div><div class="td">每次语音结束把识别用 PCM 落盘，便于排障。</div></div>
        </label>
        <label class="toggle">
          <input type="checkbox" v-model="audio.save_audio" />
          <div><div class="tl">保存合成音频</div><div class="td">额外保存 TTS 合成结果。</div></div>
        </label>
      </div>
    </div>
  </div>

  <div class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>

<style scoped>
/* Inline version badge sits right of the version string. `new` lights up in
   accent (same color as the header restart-due button) so the upgrade affordance
   reads at a glance; `ok` stays dim so the page doesn't yell "looks fine!" on
   every load. */
.status-grid .v .ver-badge{margin-left:8px;font-size:10px;letter-spacing:.05em;
  border:1px solid var(--ghost);border-radius:99px;padding:2px 8px;color:var(--dim);white-space:nowrap}
.status-grid .v .ver-badge.new{color:#04110f;background:var(--accent);border-color:var(--accent);font-weight:600}
.status-grid .v .ver-badge.ok{color:var(--dim)}
/* Action row under the identity grid — gap and msg styling match save-row so
   the buttons + status text align with the bottom 保存 row. */
.upgrade-row{display:flex;align-items:center;gap:11px;margin-top:14px;flex-wrap:wrap}
.upgrade-row .msg{margin-top:0}
</style>
