<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { getSettings, putSettings, type AsrSettings, type TtsSettings } from "../api";

// 「语音」= 声音在哪处理 + 怎么识别/合成。顶部的云/本地总开关本质就是"语音处理位置"
// （顺带定了设备接入方式：云端→经云中转，本地→LAN 直连）。云端模式语音由云端做，
// 本地无需配置；本地模式才配 ASR / TTS / 音频留存。
const emit = defineEmits<{ (e: "saved"): void }>();

const DEFAULT_ASR = {
  base_url: "https://api.openai.com",
  model: "gpt-4o-mini-transcribe",
  ws_url: "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream",
  resource_id: "volc.bigasr.sauc.duration",
  language: "zh-CN",
};
const DEFAULT_TTS = {
  ws_url: "wss://openspeech.bytedance.com/api/v1/tts/ws_binary",
  cluster: "volcano_tts",
  voice: "zh_female_wanwanxiaohe_moon_bigtts",
  local_output_format: "wav",
};

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const mode = ref<"cloud" | "local">("cloud");
const cloudAdvanced = ref(false);
const asrAdvanced = ref(false);
const ttsAdvanced = ref(false);

const cloud = reactive({ ws_url: "", auth_token: "", home_site_id: "" });
const asr = reactive<AsrSettings>({
  provider: "openai_compatible", base_url: "", ws_url: "", app_id: "", api_key: "",
  resource_id: "", model: "", language: "", local_bin: "", local_args: "", local_text_path: "",
});
const tts = reactive<TtsSettings>({
  provider: "doubao_native", token: "", app_id: "", cluster: "", voice: "",
  ws_url: "", local_bin: "", local_args: "", local_output_format: "",
});
const audio = reactive({ save_audio: false, save_input_on_finish: true });

const isLocal = computed(() => mode.value === "local");

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

function normalizeAsr(): AsrSettings {
  const next = { ...asr };
  if (next.provider === "openai_compatible") {
    next.base_url = next.base_url || DEFAULT_ASR.base_url;
    next.model = next.model || DEFAULT_ASR.model;
  } else if (next.provider === "doubao_native") {
    next.ws_url = next.ws_url || DEFAULT_ASR.ws_url;
    next.resource_id = next.resource_id || DEFAULT_ASR.resource_id;
    next.language = next.language || DEFAULT_ASR.language;
  }
  return next;
}

function normalizeTts(): TtsSettings {
  const next = { ...tts };
  if (next.provider === "doubao_native" || next.provider === "") {
    next.provider = "doubao_native";
    next.ws_url = next.ws_url || DEFAULT_TTS.ws_url;
    next.cluster = next.cluster || DEFAULT_TTS.cluster;
    next.voice = next.voice || DEFAULT_TTS.voice;
  } else if (next.provider === "local_command") {
    next.local_output_format = next.local_output_format || DEFAULT_TTS.local_output_format;
  }
  return next;
}

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(cloud, settings.cloud);
    Object.assign(asr, settings.voice.asr);
    Object.assign(tts, settings.voice.tts);
    audio.save_audio = settings.voice.save_audio;
    audio.save_input_on_finish = settings.voice.save_input_on_finish;
    mode.value = settings.topology.local_voice_enabled ? "local" : "cloud";
    if (!isLocal.value && asr.provider === "local" && !asr.local_bin.trim()) asr.provider = "doubao_native";
    if (!tts.provider) tts.provider = "doubao_native";
    loaded.value = true;
  } catch (e: any) { setNote("加载失败：" + e.message, true); }
}

async function save() {
  if (busy.value) return;
  busy.value = true;
  try {
    const topology = isLocal.value
      ? { cloud_relay_enabled: false, local_voice_enabled: true }
      : { cloud_relay_enabled: true, local_voice_enabled: false };
    const res = await putSettings({
      topology,
      cloud: { ...cloud },
      voice: {
        asr: normalizeAsr(), tts: normalizeTts(),
        save_audio: audio.save_audio, save_input_on_finish: audio.save_input_on_finish,
      },
    });
    if (isLocal.value && res.voice_incomplete)
      setNote("已保存，但本地语音还差必填项，补全后重启生效。", true);
    else
      setNote("已保存。切换处理位置 / 端点的改动需重启适配器后生效。", false);
    emit("saved");
  } catch (e: any) { setNote("保存失败：" + e.message, true); }
  finally { busy.value = false; }
}

onMounted(load);
</script>

<template>
  <!-- 总开关：语音（及设备接入）在云端还是本地 -->
  <div class="card">
    <h2>语音处理位置</h2>
    <p class="hint">这是总开关：决定语音由云端还是本机处理（设备接入方式随之而定）。选一个即可。</p>
    <div class="modes">
      <label class="mode" :class="{ on: mode === 'cloud' }">
        <input type="radio" value="cloud" v-model="mode" />
        <div>
          <div class="mt">☁ 云端处理（推荐）</div>
          <div class="md">ASR / TTS 由云端完成，本地无需任何配置；设备经云端到达本机。开箱即用。</div>
        </div>
      </label>
      <label class="mode" :class="{ on: mode === 'local' }">
        <input type="radio" value="local" v-model="mode" />
        <div>
          <div class="mt">⌂ 本地处理</div>
          <div class="md">设备 LAN 直连本机，语音在本机识别 / 合成。需在下方填 ASR / TTS。</div>
        </div>
      </label>
    </div>
  </div>

  <!-- 云端模式：无需配置，仅暴露自建云端点（高级） -->
  <div v-if="!isLocal" class="card">
    <p class="hint ok-hint">✓ 云端模式下语音全部由云端处理，本地无需配置。在 daboluo.cc 输入配对码即可激活设备。</p>
    <button class="disclose" @click="cloudAdvanced = !cloudAdvanced">
      {{ cloudAdvanced ? "▾" : "▸" }} 高级：自建云 relay 端点<span class="hint" style="margin:0 0 0 8px">（一般不用动）</span>
    </button>
    <div v-if="cloudAdvanced" class="subsec">
      <p class="hint" style="margin:0 0 10px">默认指向生产云，开箱即用；只有自建云端时才需要改。</p>
      <div class="form">
        <div class="field"><label>云端 WS 地址</label>
          <input type="text" v-model="cloud.ws_url" placeholder="wss://bbclaw.daboluo.cc/ws" /></div>
        <div class="field"><label>Auth Token</label>
          <input type="text" v-model="cloud.auth_token" placeholder="云端关闭匿名接入时才需要" /></div>
      </div>
    </div>
  </div>

  <!-- 本地模式：ASR / TTS / 音频留存 -->
  <template v-else>
    <div class="card">
      <h2>识别 / 合成（ASR · TTS）</h2>
      <p class="hint">只填服务商要求的必填项；端点、模型、语言等默认值已内置。</p>
      <div class="voice-grid">
        <div class="subsec">
          <div class="lbl">ASR · 语音识别</div>
          <div class="provider-grid">
            <label class="provider" :class="{ on: asr.provider === 'doubao_native' }">
              <input type="radio" value="doubao_native" v-model="asr.provider" /><span>豆包</span>
            </label>
            <label class="provider" :class="{ on: asr.provider === 'openai_compatible' }">
              <input type="radio" value="openai_compatible" v-model="asr.provider" /><span>OpenAI 兼容</span>
            </label>
            <label class="provider" :class="{ on: asr.provider === 'local' }">
              <input type="radio" value="local" v-model="asr.provider" /><span>本地 CLI</span>
            </label>
          </div>
          <div class="form slim-form">
            <template v-if="asr.provider === 'openai_compatible'">
              <div class="field"><label>API Key</label><input type="password" v-model="asr.api_key" /></div>
            </template>
            <template v-else-if="asr.provider === 'doubao_native'">
              <div class="row2">
                <div class="field"><label>App ID</label><input type="text" v-model="asr.app_id" /></div>
                <div class="field"><label>API Key</label><input type="password" v-model="asr.api_key" /></div>
              </div>
            </template>
            <template v-else>
              <div class="field"><label>Local Bin</label><input type="text" v-model="asr.local_bin" placeholder="/usr/local/bin/whisper" /></div>
            </template>
            <button class="disclose mini" @click="asrAdvanced = !asrAdvanced">{{ asrAdvanced ? "▾" : "▸" }} ASR 高级</button>
            <div v-if="asrAdvanced" class="advanced-box">
              <template v-if="asr.provider === 'openai_compatible'">
                <div class="field"><label>Base URL</label><input type="text" v-model="asr.base_url" :placeholder="DEFAULT_ASR.base_url" /></div>
                <div class="field"><label>Model</label><input type="text" v-model="asr.model" :placeholder="DEFAULT_ASR.model" /></div>
              </template>
              <template v-else-if="asr.provider === 'doubao_native'">
                <div class="field"><label>WS URL</label><input type="text" v-model="asr.ws_url" :placeholder="DEFAULT_ASR.ws_url" /></div>
                <div class="row2">
                  <div class="field"><label>Resource ID</label><input type="text" v-model="asr.resource_id" :placeholder="DEFAULT_ASR.resource_id" /></div>
                  <div class="field"><label>Language</label><input type="text" v-model="asr.language" :placeholder="DEFAULT_ASR.language" /></div>
                </div>
              </template>
              <template v-else>
                <div class="row2">
                  <div class="field"><label>Local Args</label><input type="text" v-model="asr.local_args" placeholder="--model base --language zh" /></div>
                  <div class="field"><label>Text Output Path</label><input type="text" v-model="asr.local_text_path" placeholder="可选" /></div>
                </div>
              </template>
            </div>
          </div>
        </div>

        <div class="subsec">
          <div class="lbl">TTS · 语音合成</div>
          <div class="provider-grid">
            <label class="provider" :class="{ on: tts.provider === 'doubao_native' || tts.provider === '' }">
              <input type="radio" value="doubao_native" v-model="tts.provider" /><span>豆包</span>
            </label>
            <label class="provider" :class="{ on: tts.provider === 'local_command' }">
              <input type="radio" value="local_command" v-model="tts.provider" /><span>本地 CLI</span>
            </label>
            <label class="provider" :class="{ on: tts.provider === 'mock' }">
              <input type="radio" value="mock" v-model="tts.provider" /><span>静音测试</span>
            </label>
          </div>
          <div class="form slim-form">
            <template v-if="tts.provider === 'doubao_native' || tts.provider === ''">
              <div class="row2">
                <div class="field"><label>App ID</label><input type="text" v-model="tts.app_id" /></div>
                <div class="field"><label>Token</label><input type="password" v-model="tts.token" /></div>
              </div>
            </template>
            <template v-else-if="tts.provider === 'local_command'">
              <div class="field"><label>Local Bin</label><input type="text" v-model="tts.local_bin" placeholder="/usr/bin/say" /></div>
            </template>
            <p v-else class="hint">仅用于冒烟测试，不做真实合成。</p>
            <button class="disclose mini" @click="ttsAdvanced = !ttsAdvanced">{{ ttsAdvanced ? "▾" : "▸" }} TTS 高级</button>
            <div v-if="ttsAdvanced" class="advanced-box">
              <template v-if="tts.provider === 'doubao_native' || tts.provider === ''">
                <div class="field"><label>WS URL</label><input type="text" v-model="tts.ws_url" :placeholder="DEFAULT_TTS.ws_url" /></div>
                <div class="row2">
                  <div class="field"><label>Cluster</label><input type="text" v-model="tts.cluster" :placeholder="DEFAULT_TTS.cluster" /></div>
                  <div class="field"><label>Voice</label><input type="text" v-model="tts.voice" :placeholder="DEFAULT_TTS.voice" /></div>
                </div>
              </template>
              <template v-else-if="tts.provider === 'local_command'">
                <div class="row2">
                  <div class="field"><label>Local Args</label><input type="text" v-model="tts.local_args" placeholder="-o {out} {text}" /></div>
                  <div class="field"><label>Output Format</label><input type="text" v-model="tts.local_output_format" :placeholder="DEFAULT_TTS.local_output_format" /></div>
                </div>
              </template>
              <p v-else class="hint">静音测试没有高级项。</p>
            </div>
          </div>
        </div>
      </div>
    </div>

    <div class="card">
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
  </template>

  <div class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存语音配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
