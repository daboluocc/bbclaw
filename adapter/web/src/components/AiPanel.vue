<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import DriversPanel from "./DriversPanel.vue";
import ProjectsPanel from "./ProjectsPanel.vue";
import { getSettings, putSettings, type AsrSettings, type TtsSettings } from "../api";

const emit = defineEmits<{ (e: "saved"): void }>();
const props = defineProps<{ localVoiceOverride?: boolean | null }>();

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
const localVoice = ref(false); // owned by 系统配置; read-only here, gates ASR/TTS
const localVoiceEnabled = computed(() => props.localVoiceOverride ?? localVoice.value);
const claudeAdvanced = ref(false);
const asrAdvanced = ref(false);
const ttsAdvanced = ref(false);

const ai = reactive({ anthropic_base_url: "", anthropic_auth_token: "", opencode_serve: false });
const asr = reactive<AsrSettings>({
  provider: "openai_compatible", base_url: "", ws_url: "", app_id: "", api_key: "",
  resource_id: "", model: "", language: "", local_bin: "", local_args: "", local_text_path: "",
});
const tts = reactive<TtsSettings>({
  provider: "doubao_native", token: "", app_id: "", cluster: "", voice: "",
  ws_url: "", local_bin: "", local_args: "", local_output_format: "",
});

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

function preferMainstreamVoiceDefaults(activeLocalVoice: boolean) {
  if (!activeLocalVoice && asr.provider === "local" && !asr.local_bin.trim()) {
    asr.provider = "doubao_native";
  }
  if (!tts.provider) tts.provider = "doubao_native";
}

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(ai, settings.ai);
    Object.assign(asr, settings.voice.asr);
    Object.assign(tts, settings.voice.tts);
    localVoice.value = settings.topology.local_voice_enabled;
    preferMainstreamVoiceDefaults(localVoice.value);
    loaded.value = true;
  } catch (e: any) { setNote("加载失败：" + e.message, true); }
}

async function save() {
  if (busy.value) return;
  busy.value = true;
  try {
    const res = await putSettings({ ai: { ...ai }, voice: { asr: normalizeAsr(), tts: normalizeTts() } });
    if (res.voice_incomplete)
      setNote("已保存，但本地语音还差必填项。补全后重启生效。", true);
    else
      setNote("已保存。需要重启适配器后生效。", false);
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
    <h2>opencode 后端（ADR-031）</h2>
    <p class="hint">
      开启后，opencode 走常驻 <code>opencode serve</code> + SDK（原生流式、可中断、历史回放、
      模型/会话列举）；关闭则用旧的「每轮 spawn <code>opencode run</code>」CLI 通路。
      <b>切换需重启适配器后生效。</b>
    </p>
    <label class="toggle">
      <input type="checkbox" v-model="ai.opencode_serve" />
      <span class="tl">使用 serve + SDK 后端</span>
    </label>
  </div>

  <div class="card">
    <button class="disclose" @click="claudeAdvanced = !claudeAdvanced">
      {{ claudeAdvanced ? "▾" : "▸" }} Claude 代理 / 兼容端点
      <span class="hint" style="margin:0 0 0 8px">（留空使用默认登录态）</span>
    </button>
    <div v-if="claudeAdvanced" class="subsec">
      <div class="form">
        <div class="field"><label>Base URL</label>
          <input type="text" v-model="ai.anthropic_base_url" placeholder="https://your-proxy.example.com" /></div>
        <div class="field"><label>Auth Token</label>
          <input type="password" v-model="ai.anthropic_auth_token" placeholder="sk-..." /></div>
      </div>
    </div>
  </div>

  <!-- 项目白名单：管家可派活的目录，运行时增删，不走重启 -->
  <ProjectsPanel />

  <div v-if="!localVoiceEnabled" class="card quiet-card">
    <h2>本地语音</h2>
    <p class="hint">
      当前是<b style="color:var(--accent)">云端模式</b>，ASR/TTS 由云端处理，本地不需要配置。
    </p>
  </div>

  <div v-else class="card">
    <h2>本地语音</h2>
    <p class="hint">只填服务商要求的必填项；端点、模型、语言等默认值已内置。</p>

    <div class="voice-grid">
      <div class="subsec">
        <div class="lbl">ASR · 语音识别</div>
        <div class="provider-grid">
          <label class="provider" :class="{ on: asr.provider === 'doubao_native' }">
            <input type="radio" value="doubao_native" v-model="asr.provider" />
            <span>豆包</span>
          </label>
          <label class="provider" :class="{ on: asr.provider === 'openai_compatible' }">
            <input type="radio" value="openai_compatible" v-model="asr.provider" />
            <span>OpenAI 兼容</span>
          </label>
          <label class="provider" :class="{ on: asr.provider === 'local' }">
            <input type="radio" value="local" v-model="asr.provider" />
            <span>本地 CLI</span>
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
          <button class="disclose mini" @click="asrAdvanced = !asrAdvanced">
            {{ asrAdvanced ? "▾" : "▸" }} ASR 高级
          </button>
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
            <input type="radio" value="doubao_native" v-model="tts.provider" />
            <span>豆包</span>
          </label>
          <label class="provider" :class="{ on: tts.provider === 'local_command' }">
            <input type="radio" value="local_command" v-model="tts.provider" />
            <span>本地 CLI</span>
          </label>
          <label class="provider" :class="{ on: tts.provider === 'mock' }">
            <input type="radio" value="mock" v-model="tts.provider" />
            <span>静音测试</span>
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
          <button class="disclose mini" @click="ttsAdvanced = !ttsAdvanced">
            {{ ttsAdvanced ? "▾" : "▸" }} TTS 高级
          </button>
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
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存 AI 配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
