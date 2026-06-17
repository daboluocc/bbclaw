<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import DriversPanel from "./DriversPanel.vue";
import ProjectsPanel from "./ProjectsPanel.vue";
import { getSettings, putSettings } from "../api";

// 「智能体」分类，按层级组织：
//   1. 驱动（Driver 层）——选用哪个 CLI；
//   2. 当前激活驱动的专属配置——只显示该驱动需要的项（claude 配三方端点、
//      opencode 选 serve 后端…），不再把所有驱动的配置平铺；
//   3. 项目白名单——跨驱动，管家可派活的目录。
const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const claudeAdvanced = ref(false);
const activeDriver = ref("");

const ai = reactive({ anthropic_base_url: "", anthropic_auth_token: "", opencode_serve: false });

// Which driver has its own config card here. Other drivers (ollama/aider/codex/
// openclaw) carry credentials in their own login state/env — nothing to set.
const hasDriverConfig = computed(
  () => activeDriver.value === "claude-code" || activeDriver.value === "opencode",
);

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
    setNote("已保存。需重启适配器后生效。", false);
    emit("saved");
  } catch (e: any) { setNote("保存失败：" + e.message, true); }
  finally { busy.value = false; }
}

onMounted(load);
</script>

<template>
  <!-- 1. Driver 层：选用哪个 CLI（切换即时生效，不走重启） -->
  <DriversPanel @active="activeDriver = $event" />

  <!-- 2. 当前激活驱动的专属配置 -->
  <div v-if="activeDriver === 'opencode'" class="card">
    <h2>opencode 配置</h2>
    <p class="hint">
      serve 后端：常驻 <code>opencode serve</code> + SDK（原生流式、可中断、历史回放、
      模型/会话列举）；关闭则用旧的「每轮 spawn <code>opencode run</code>」CLI 通路。
      <b>切换需重启后生效。</b>
    </p>
    <label class="toggle">
      <input type="checkbox" v-model="ai.opencode_serve" />
      <span class="tl">使用 serve + SDK 后端</span>
    </label>
  </div>

  <div v-else-if="activeDriver === 'claude-code'" class="card">
    <h2>claude 配置</h2>
    <p class="hint">连官方登录态时这里留空即可；只有走代理 / 兼容端点才需要填。</p>
    <button class="disclose" @click="claudeAdvanced = !claudeAdvanced">
      {{ claudeAdvanced ? "▾" : "▸" }} Claude 代理 / 兼容端点
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

  <div v-else-if="activeDriver" class="card quiet-card">
    <p class="hint">
      <b>{{ activeDriver }}</b> 无需在此额外配置——它的凭证走自身的登录态 / 环境变量。
    </p>
  </div>

  <!-- 3. 项目白名单：跨驱动，管家可派活的目录（运行时增删，不走重启） -->
  <ProjectsPanel />

  <div v-if="hasDriverConfig" class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存 {{ activeDriver }} 配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
