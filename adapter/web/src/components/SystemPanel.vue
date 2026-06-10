<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import StatusBar from "./StatusBar.vue";
import { getSettings, putSettings } from "../api";

const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);

// Editable slices this page owns (topology + cloud + openclaw + audio toggles).
const topology = reactive({ cloud_relay_enabled: true, local_voice_enabled: false });
const cloud = reactive({ ws_url: "", auth_token: "", home_site_id: "" });
const openclaw = reactive({ ws_url: "", auth_token: "", node_id: "" });
const audio = reactive({ save_audio: false, save_input_on_finish: true });

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(topology, settings.topology);
    Object.assign(cloud, settings.cloud);
    Object.assign(openclaw, settings.openclaw);
    audio.save_audio = settings.voice.save_audio;
    audio.save_input_on_finish = settings.voice.save_input_on_finish;
    loaded.value = true;
  } catch (e: any) { setNote("加载失败：" + e.message, true); }
}

async function save() {
  if (busy.value) return;
  busy.value = true;
  try {
    await putSettings({
      topology: { ...topology },
      cloud: { ...cloud },
      openclaw: { ...openclaw },
      voice: { save_audio: audio.save_audio, save_input_on_finish: audio.save_input_on_finish },
    });
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
    <h2>部署形态</h2>
    <p class="hint">默认走云端：设备语音经云端做 ASR/TTS，本地无需配置语音。本地管理页始终可用。</p>
    <label class="toggle">
      <input type="checkbox" v-model="topology.cloud_relay_enabled" />
      <div><div class="tl">连接云端 relay</div>
        <div class="td">设备经 daboluo.cc 云端中转到本机（默认开）。关掉则只跑本地局域网直连。</div></div>
    </label>
    <label class="toggle">
      <input type="checkbox" v-model="topology.local_voice_enabled" />
      <div><div class="tl">本地语音管线（LAN 直连 ASR/TTS）</div>
        <div class="td">设备直连本机做语音时才需要。开启后请到「AI 配置」填 ASR/TTS 密钥。默认关。</div></div>
    </label>
  </div>

  <div class="card" v-if="topology.cloud_relay_enabled">
    <h2>云端 relay</h2>
    <p class="hint">默认指向生产云 wss://bbclaw.daboluo.cc/ws，匿名接入后在网页端输入配对码激活。自建云才需要改。</p>
    <div class="form">
      <div class="field"><label>云端 WS 地址</label>
        <input type="text" v-model="cloud.ws_url" placeholder="wss://bbclaw.daboluo.cc/ws" /></div>
      <div class="row2">
        <div class="field"><label>Auth Token（可选）</label>
          <input type="text" v-model="cloud.auth_token" placeholder="云端关闭匿名接入时才需要" /></div>
        <div class="field"><label>Home Site ID（可选）</label>
          <input type="text" v-model="cloud.home_site_id" placeholder="留空自动派生" /></div>
      </div>
    </div>
  </div>

  <div class="card">
    <h2>OpenClaw 网关</h2>
    <p class="hint">本地 OpenClaw 编排网关（sink + 驱动）。不跑 OpenClaw 可忽略。</p>
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

  <div class="card">
    <h2>音频留存</h2>
    <label class="toggle">
      <input type="checkbox" v-model="audio.save_input_on_finish" />
      <div><div class="tl">保存输入音频</div><div class="td">每次语音结束把识别用的 PCM 落盘，便于排障。</div></div>
    </label>
    <label class="toggle">
      <input type="checkbox" v-model="audio.save_audio" />
      <div><div class="tl">保存合成音频</div><div class="td">额外保存 TTS 合成结果。仅本地语音管线生效。</div></div>
    </label>
  </div>

  <div class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存系统配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
