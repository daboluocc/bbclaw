<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import DriversPanel from "./DriversPanel.vue";
import ProjectsPanel from "./ProjectsPanel.vue";
import { getSettings, putSettings, type AsrSettings, type TtsSettings } from "../api";

const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const localVoice = ref(false); // owned by 系统配置; read-only here, gates ASR/TTS

const ai = reactive({ anthropic_base_url: "", anthropic_auth_token: "" });
const asr = reactive<AsrSettings>({
  provider: "openai_compatible", base_url: "", ws_url: "", app_id: "", api_key: "",
  resource_id: "", model: "", language: "", local_bin: "", local_args: "", local_text_path: "",
});
const tts = reactive<TtsSettings>({
  provider: "doubao_native", token: "", app_id: "", cluster: "", voice: "",
  ws_url: "", local_bin: "", local_args: "", local_output_format: "",
});

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(ai, settings.ai);
    Object.assign(asr, settings.voice.asr);
    Object.assign(tts, settings.voice.tts);
    localVoice.value = settings.topology.local_voice_enabled;
    loaded.value = true;
  } catch (e: any) { setNote("加载失败：" + e.message, true); }
}

async function save() {
  if (busy.value) return;
  busy.value = true;
  try {
    const res = await putSettings({ ai: { ...ai }, voice: { asr: { ...asr }, tts: { ...tts } } });
    if (res.voice_incomplete)
      setNote("已保存，但 ASR/TTS 还差字段，语音暂不可用 —— 补全后重启生效。", true);
    else
      setNote("已保存。第三方端点 / 语音改动需重启适配器后生效。", false);
    emit("saved");
  } catch (e: any) { setNote("保存失败：" + e.message, true); }
  finally { busy.value = false; }
}

onMounted(load);
</script>

<template>
  <!-- 驱动单选：切换即时生效，不走重启 -->
  <DriversPanel />

  <div class="card">
    <h2>第三方 Claude 端点</h2>
    <p class="hint">把 claude 子进程指向你的代理 / 兼容端点（注入 ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN）。留空用官方 API。</p>
    <div class="form">
      <div class="field"><label>Base URL</label>
        <input type="text" v-model="ai.anthropic_base_url" placeholder="https://your-proxy.example.com" /></div>
      <div class="field"><label>Auth Token</label>
        <input type="text" v-model="ai.anthropic_auth_token" placeholder="sk-..." /></div>
    </div>
  </div>

  <!-- 项目白名单：管家可派活的目录，运行时增删，不走重启 -->
  <ProjectsPanel />

  <div class="card">
    <h2>语音（ASR / TTS）</h2>
    <p v-if="!localVoice" class="hint">
      当前是<b style="color:var(--accent)">云端模式</b> —— ASR/TTS 由云端完成，本地无需配置。
      如需设备 LAN 直连本机做语音，到「系统配置 → 部署模式」切到<b>本地模式</b>。
    </p>

    <template v-else>
      <div class="subsec">
        <div class="lbl">ASR · 语音识别</div>
        <div class="form">
          <div class="field"><label>Provider</label>
            <select v-model="asr.provider">
              <option value="openai_compatible">openai_compatible · /v1/audio/transcriptions</option>
              <option value="doubao_native">doubao_native · 豆包 OpenSpeech</option>
              <option value="local">local · 本地 CLI（whisper / FunASR）</option>
            </select>
          </div>
          <template v-if="asr.provider === 'openai_compatible'">
            <div class="field"><label>Base URL</label><input type="text" v-model="asr.base_url" placeholder="https://api.openai.com" /></div>
            <div class="row2">
              <div class="field"><label>API Key</label><input type="text" v-model="asr.api_key" /></div>
              <div class="field"><label>Model</label><input type="text" v-model="asr.model" placeholder="gpt-4o-mini-transcribe" /></div>
            </div>
          </template>
          <template v-else-if="asr.provider === 'doubao_native'">
            <div class="field"><label>WS URL</label><input type="text" v-model="asr.ws_url" /></div>
            <div class="row2">
              <div class="field"><label>App ID</label><input type="text" v-model="asr.app_id" /></div>
              <div class="field"><label>API Key</label><input type="text" v-model="asr.api_key" /></div>
            </div>
            <div class="row2">
              <div class="field"><label>Resource ID</label><input type="text" v-model="asr.resource_id" placeholder="volc.bigasr.sauc.duration" /></div>
              <div class="field"><label>Language</label><input type="text" v-model="asr.language" placeholder="zh-CN" /></div>
            </div>
          </template>
          <template v-else>
            <div class="field"><label>Local Bin</label><input type="text" v-model="asr.local_bin" placeholder="/usr/local/bin/whisper" /></div>
            <div class="row2">
              <div class="field"><label>Local Args</label><input type="text" v-model="asr.local_args" placeholder="--model base --language zh" /></div>
              <div class="field"><label>Text Output Path</label><input type="text" v-model="asr.local_text_path" placeholder="可选" /></div>
            </div>
          </template>
        </div>
      </div>

      <div class="subsec">
        <div class="lbl">TTS · 语音合成</div>
        <div class="form">
          <div class="field"><label>Provider</label>
            <select v-model="tts.provider">
              <option value="doubao_native">doubao_native · 豆包 OpenSpeech</option>
              <option value="local_command">local_command · 本地 CLI（如 macOS say）</option>
              <option value="mock">mock · 静音（仅冒烟测试）</option>
            </select>
          </div>
          <template v-if="tts.provider === 'doubao_native'">
            <div class="field"><label>WS URL</label><input type="text" v-model="tts.ws_url" /></div>
            <div class="row2">
              <div class="field"><label>App ID</label><input type="text" v-model="tts.app_id" /></div>
              <div class="field"><label>Token</label><input type="text" v-model="tts.token" /></div>
            </div>
            <div class="row2">
              <div class="field"><label>Cluster</label><input type="text" v-model="tts.cluster" placeholder="volcano_tts" /></div>
              <div class="field"><label>Voice</label><input type="text" v-model="tts.voice" placeholder="zh_female_..." /></div>
            </div>
          </template>
          <template v-else-if="tts.provider === 'local_command'">
            <div class="field"><label>Local Bin</label><input type="text" v-model="tts.local_bin" placeholder="/usr/bin/say" /></div>
            <div class="row2">
              <div class="field"><label>Local Args</label><input type="text" v-model="tts.local_args" placeholder="-o {out} {text}" /></div>
              <div class="field"><label>Output Format</label><input type="text" v-model="tts.local_output_format" placeholder="aiff" /></div>
            </div>
          </template>
          <p v-else class="hint">mock 不做真实合成，返回静音 WAV。</p>
        </div>
      </div>
    </template>
  </div>

  <div class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存 AI 配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
