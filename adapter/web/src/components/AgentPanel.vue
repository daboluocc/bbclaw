<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import DriversPanel from "./DriversPanel.vue";
import ProjectsPanel from "./ProjectsPanel.vue";
import { getSettings, putSettings } from "../api";

// 「智能体」分类：选哪个 CLI、opencode 用哪种后端、Claude 端点、可派活的项目。
// 语音、部署在各自分类页，互不混杂。
const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const claudeAdvanced = ref(false);

const ai = reactive({ anthropic_base_url: "", anthropic_auth_token: "", opencode_serve: false });

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(ai, settings.ai);
    loaded.value = true;
  } catch (e: any) { setNote("加载失败：" + e.message, true); }
}

async function save() {
  if (busy.value) return;
  busy.value = true;
  try {
    await putSettings({ ai: { ...ai } });
    setNote("已保存。Claude 端点 / opencode 后端的改动需重启适配器后生效。", false);
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

  <div class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存智能体配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
