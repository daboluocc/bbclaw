<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import DriversPanel from "./DriversPanel.vue";
import ProjectsPanel from "./ProjectsPanel.vue";
import { getSettings, putSettings } from "../api";

// 「智能体」分层：
//   1. 驱动（Driver 层）——选用哪个 CLI（热切换，即时生效）；
//   2. 当前激活驱动的【运行时】配置——只在该驱动激活时才相关（如 claude 的三方端点）；
//   3. opencode 后端——驱动的【结构性】运行模式，跟当前激活哪个无关，始终可预先配置
//      （切换需重启，因为 driver 实例在启动时构造）；
//   4. 项目白名单——跨驱动，管家可派活的目录。
const emit = defineEmits<{ (e: "saved"): void }>();

const loaded = ref(false);
const busy = ref(false);
const note = ref("");
const noteErr = ref(false);
const claudeAdvanced = ref(false);
const activeDriver = ref("");

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
  <!-- 1. Driver 层：选用哪个 CLI（切换即时生效，不走重启） -->
  <DriversPanel @active="activeDriver = $event" />

  <!-- 2. 当前激活驱动的运行时配置（只在该驱动激活时相关） -->
  <div v-if="activeDriver === 'claude-code'" class="card">
    <h2>claude 配置</h2>
    <p class="hint">连官方登录态时留空即可；只有走代理 / 兼容端点才需要填。</p>
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
  <div v-else-if="activeDriver && activeDriver !== 'opencode'" class="card quiet-card">
    <p class="hint"><b>{{ activeDriver }}</b> 无需额外运行时配置——凭证走自身的登录态 / 环境变量。</p>
  </div>

  <!-- 3. opencode 后端：结构性运行模式，始终可见、可预先配置（需重启生效） -->
  <div class="card">
    <h2>opencode 后端</h2>
    <p class="hint">
      opencode 驱动的运行模式，<b>无论当前激活哪个驱动都可预先配置</b>，激活 opencode 时按此运行。
      serve：常驻 <code>opencode serve</code> + SDK（原生流式、可中断、历史回放、模型/会话列举）；
      关闭则用旧的「每轮 spawn <code>opencode run</code>」CLI 通路。<b>切换需重启生效。</b>
    </p>
    <label class="toggle">
      <input type="checkbox" v-model="ai.opencode_serve" />
      <span class="tl">使用 serve + SDK 后端</span>
    </label>
  </div>

  <!-- 4. 项目白名单：跨驱动，管家可派活的目录（运行时增删，不走重启） -->
  <ProjectsPanel />

  <div class="card">
    <div class="save-row">
      <button :disabled="busy || !loaded" @click="save">保存智能体配置</button>
      <span class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</span>
    </div>
  </div>
</template>
