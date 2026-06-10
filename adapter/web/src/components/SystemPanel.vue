<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import StatusBar from "./StatusBar.vue";
import { getSettings, putSettings } from "../api";

const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const advanced = ref(false);

// One choice drives the page: cloud (cloud does ASR/TTS, local needs nothing) or
// local (device LAN-direct, you configure ASR/TTS on the AI page).
const mode = ref<"cloud" | "local">("cloud");
const cloud = reactive({ ws_url: "", auth_token: "", home_site_id: "" });
const openclaw = reactive({ ws_url: "", auth_token: "", node_id: "" });
const audio = reactive({ save_audio: false, save_input_on_finish: true });

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(cloud, settings.cloud);
    Object.assign(openclaw, settings.openclaw);
    audio.save_audio = settings.voice.save_audio;
    audio.save_input_on_finish = settings.voice.save_input_on_finish;
    mode.value = settings.topology.local_voice_enabled ? "local" : "cloud";
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
      openclaw: { ...openclaw },
      voice: { save_audio: audio.save_audio, save_input_on_finish: audio.save_input_on_finish },
    });
    if (mode.value === "local" && res.voice_incomplete)
      setNote("已保存。但 ASR/TTS 还没填完整 → 去「AI 配置」填好后语音才可用。", true);
    else
      setNote("已保存。重启适配器后生效。", false);
    emit("saved");
  } catch (e: any) { setNote("保存失败：" + e.message, true); }
  finally { busy.value = false; }
}

onMounted(load);
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
          <div class="md">设备 LAN 直连本机，语音在本机处理。需要在「AI 配置」填写 ASR / TTS 密钥。</div>
        </div>
      </label>
    </div>

    <p v-if="mode === 'cloud'" class="hint ok-hint">
      ✓ 无需本地配置。在网页端 daboluo.cc 输入配对码即可激活设备。
    </p>
    <p v-else class="hint">下一步 → 到「AI 配置」页填写 ASR / TTS，否则语音不可用。</p>
  </div>

  <div class="card">
    <button class="disclose" @click="advanced = !advanced">
      {{ advanced ? "▾" : "▸" }} 高级设置<span class="hint" style="margin:0 0 0 8px">（一般不用动）</span>
    </button>

    <div v-if="advanced">
      <div class="subsec" v-if="mode === 'cloud'">
        <div class="lbl">云端 relay</div>
        <div class="form">
          <div class="field"><label>云端 WS 地址</label>
            <input type="text" v-model="cloud.ws_url" placeholder="wss://bbclaw.daboluo.cc/ws" />
            <div class="fh">默认指向生产云，自建云才需要改</div></div>
          <div class="row2">
            <div class="field"><label>Auth Token</label>
              <input type="text" v-model="cloud.auth_token" placeholder="云端关闭匿名接入时才需要" /></div>
            <div class="field"><label>Home Site ID</label>
              <input type="text" v-model="cloud.home_site_id" placeholder="留空自动派生" /></div>
          </div>
        </div>
      </div>

      <div class="subsec">
        <div class="lbl">OpenClaw 网关</div>
        <div class="form">
          <div class="field"><label>WS 地址</label>
            <input type="text" v-model="openclaw.ws_url" placeholder="ws://127.0.0.1:18789" /></div>
          <div class="row2">
            <div class="field"><label>Auth Token</label>
              <input type="text" v-model="openclaw.auth_token" placeholder="可选" /></div>
            <div class="field"><label>Node ID</label>
              <input type="text" v-model="openclaw.node_id" placeholder="bbclaw-adapter" /></div>
          </div>
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
