<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import DriversPanel from "./DriversPanel.vue";
import ProjectsPanel from "./ProjectsPanel.vue";
import { getSettings, putSettings, listModels, setActiveModel, type ModelInfo } from "../api";

// 「智能体」分层：
//   1. 驱动（Driver 层）——选用哪个 CLI（热切换，即时生效）；
//   2. claude 端点配置——claude-code 驱动的三方 / 代理端点。【始终可配置】：claude-code
//      既能被直接激活，也会在当前驱动非 butler-capable 时作为设备管家回落驱动运行
//      （ADR-023），所以即便当前激活的是别的驱动也得能预填——不能再藏在「激活才显示」
//      后面（回归 v1 行为）；
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
const claudeModels = ref<ModelInfo[]>([]);
const activeClaudeModel = ref("");
const loadingModels = ref(false);

const ai = reactive({ anthropic_base_url: "", anthropic_auth_token: "", opencode_serve: false });

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  try {
    const { settings } = await getSettings();
    Object.assign(ai, settings.ai);
    // 已经配过三方端点就默认展开，免得用户以为配置丢了。
    if (ai.anthropic_base_url || ai.anthropic_auth_token) claudeAdvanced.value = true;

    // 加载 Claude Code 的模型列表
    try {
      loadingModels.value = true;
      const menu = await listModels("claude-code");
      claudeModels.value = menu.models;
      activeClaudeModel.value = menu.active;
    } catch (e) {
      // 加载模型列表失败不应该中断整体流程
      console.warn("加载 Claude 模型列表失败:", e);
    } finally {
      loadingModels.value = false;
    }

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

async function selectClaudeModel(modelId: string) {
  if (activeClaudeModel.value === modelId) return;
  try {
    loadingModels.value = true;
    await setActiveModel("claude-code", modelId);
    activeClaudeModel.value = modelId;
    setNote(`已选择 ${claudeModels.value.find(m => m.id === modelId)?.label || modelId}`, false);
  } catch (e: any) {
    setNote("选择模型失败：" + e.message, true);
  } finally {
    loadingModels.value = false;
  }
}

onMounted(load);
</script>

<template>
  <!-- 1. Driver 层：选用哪个 CLI（切换即时生效，不走重启） -->
  <DriversPanel @active="activeDriver = $event" />

  <!-- 2. claude 端点配置：claude-code 驱动的代理 / 兼容端点，始终可配置。
       claude-code 既能被直接激活，也会在当前驱动非 butler-capable 时作为设备管家
       回落驱动运行（ADR-023），所以任意激活驱动下都应能预填这里。 -->
  <div class="card">
    <h2>claude 配置</h2>
    <p class="hint">
      连官方登录态时留空即可；只有走代理 / 兼容端点才需要填。
      <span v-if="activeDriver && activeDriver !== 'claude-code'">
        当前激活的是 <b>{{ activeDriver }}</b>，但 claude-code 仍可能作为设备管家回落驱动运行，故此处始终可配置。
      </span>
    </p>

    <!-- 模型选择 -->
    <div v-if="claudeModels.length > 0" class="subsec models-section">
      <label class="label-text">模型选择</label>
      <div class="model-grid">
        <button
          v-for="m in claudeModels" :key="m.id"
          class="model-btn"
          :class="{ selected: activeClaudeModel === m.id, loading: loadingModels }"
          :disabled="loadingModels"
          @click="selectClaudeModel(m.id)"
        >
          {{ m.label }}
        </button>
      </div>
      <p v-if="activeClaudeModel" class="hint" style="margin-top: 8px;">
        当前选择：<b>{{ claudeModels.find(m => m.id === activeClaudeModel)?.label }}</b>
      </p>
    </div>

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

<style scoped>
.label-text {
  display: block;
  font-size: 12px;
  font-weight: 600;
  letter-spacing: 0.04em;
  color: var(--lit);
  margin-bottom: 10px;
  text-transform: uppercase;
}

.model-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: 8px;
  margin-bottom: 8px;
}

.model-btn {
  padding: 8px 12px;
  font-size: 12px;
  font-weight: 500;
  border: 1px solid var(--ghost);
  border-radius: 6px;
  background: rgba(7, 11, 14, 0.5);
  color: var(--dim);
  cursor: pointer;
  transition: all 0.15s ease;
  text-align: center;
  font-family: inherit;
}

.model-btn:hover:not(:disabled) {
  border-color: var(--dim);
  color: var(--lit);
  background: rgba(13, 18, 22, 0.7);
}

.model-btn.selected {
  border-color: var(--accent);
  background: rgba(46, 196, 160, 0.15);
  color: var(--accent);
  font-weight: 600;
  box-shadow: 0 0 0 1px rgba(46, 196, 160, 0.3);
}

.model-btn:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.models-section {
  padding: 12px 0;
  border-bottom: 1px solid rgba(110, 138, 147, 0.2);
  margin-bottom: 12px;
}
</style>
