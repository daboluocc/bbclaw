<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { drivers, setActiveDriver, type DriverRow } from "../api";

// Driver 描述字典 — 为每个已知 driver 提供友好的描述文字
const DRIVER_DESCRIPTIONS: Record<string, string> = {
  "claude-code": "Claude Code 官方 CLI，全能力支持",
  "opencode": "开源 AI 代理，支持多模型后端",
  "aider": "终端原生 AI 编码助手",
  "ollama": "本地大模型推理，离线可用",
};

// Capability 中文标签和说明
const CAPABILITY_LABELS: Record<string, { label: string; hint: string }> = {
  butler: { label: "管家", hint: "支持设备管家模式（语音交互、任务派发）" },
  resume: { label: "恢复", hint: "支持会话恢复，保持对话连续性" },
  streaming: { label: "流式", hint: "支持流式响应，实时展示生成过程" },
};

// Report the active driver up so the parent can show only that driver's
// settings (Driver is the top layer; per-driver config nests under it).
const emit = defineEmits<{ (e: "active", name: string): void }>();

const rows = ref<DriverRow[]>([]);
const activeDriver = ref("");
const butlerDriver = ref("");
const note = ref("");
const noteErr = ref(false);
const busy = ref(false);

function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function load() {
  const d = await drivers();
  rows.value = d.drivers;
  activeDriver.value = d.active_driver;
  butlerDriver.value = d.butler_driver;
  emit("active", activeDriver.value);
}

function installed(r: DriverRow): boolean { return r.installed !== false; }

function getDescription(name: string): string {
  return DRIVER_DESCRIPTIONS[name] || "AI 代理驱动";
}

// 解析 capabilities 对象为数组
function getCapabilities(r: DriverRow): Array<{ key: string; supported: boolean }> {
  const caps = r.capabilities || {};
  return [
    { key: "butler", supported: caps.butler ?? false },
    { key: "resume", supported: caps.resume ?? false },
    { key: "streaming", supported: caps.streaming ?? false },
  ];
}

async function pick(r: DriverRow) {
  if (busy.value || !installed(r) || r.name === activeDriver.value) return;

  // 切换确认（如果当前有活跃驱动）
  if (activeDriver.value && !confirm(
    `切换到 ${r.name} 会重置对话连续性，下一轮对话将使用新驱动。\n\n确定继续？`
  )) return;

  busy.value = true;
  try {
    await setActiveDriver(r.name);
    setNote(`已切到 ${r.name}，下一轮对话生效`, false);
    await load();
  } catch (e: any) { setNote("切换失败：" + e.message, true); }
  finally { busy.value = false; }
}

// The device butler resolves to active_driver when butler-capable, else falls
// back to claude-code. Surface that so the user understands a non-butler pick.
const fallbackActive = computed(() => {
  const a = rows.value.find((r) => r.name === activeDriver.value);
  return a != null && a.butler_capable === false;
});
</script>

<template>
  <div class="card">
    <h2>驱动（设备管家用哪个 CLI）</h2>
    <p class="hint">
      整套都用激活的驱动：设备管家、派活的 worker、记忆都跟随它。未安装的灰掉无法选；
      切换后下一轮对话生效。不是 butler-capable 的驱动选中后，设备管家会回落到 claude-code。
    </p>

    <div class="driver-grid">
      <button
        v-for="r in rows" :key="r.name"
        class="driver-card"
        :class="{
          active: r.name === activeDriver,
          missing: !installed(r),
          'has-warning': r.warning
        }"
        :disabled="busy || !installed(r)"
        @click="pick(r)"
      >
        <!-- Header: 名称 + 状态徽章 -->
        <div class="dc-header">
          <div class="dc-name">{{ r.name }}</div>
          <div class="dc-badges">
            <span v-if="r.warning" class="dc-badge warn" title="有警告">⚠</span>
            <span v-if="r.name === activeDriver" class="dc-badge current">当前</span>
            <span v-else-if="!installed(r)" class="dc-badge missing">未安装</span>
            <span v-else-if="r.butler_capable === false" class="dc-badge info">非管家</span>
            <span v-else class="dc-badge ok">已安装</span>
          </div>
        </div>

        <!-- Description + Model -->
        <div class="dc-desc">
          {{ getDescription(r.name) }}
          <span v-if="r.active_model" class="dc-model">· {{ r.active_model }}</span>
        </div>

        <!-- Capabilities 行 -->
        <div class="dc-caps">
          <span
            v-for="cap in getCapabilities(r)" :key="cap.key"
            class="dc-cap"
            :class="{ on: cap.supported, off: !cap.supported }"
            :title="CAPABILITY_LABELS[cap.key]?.hint || ''"
          >
            <span class="dc-cap-icon">{{ cap.supported ? '✓' : '✗' }}</span>
            <span class="dc-cap-label">{{ CAPABILITY_LABELS[cap.key]?.label || cap.key }}</span>
          </span>
        </div>
      </button>

      <div v-if="!rows.length" class="empty-state">
        <div class="empty-icon">◇</div>
        <div class="empty-text">没有已注册驱动</div>
      </div>
    </div>

    <!-- Warning 详情面板 -->
    <div v-if="rows.some(r => r.warning)" class="warnings-panel">
      <div v-for="r in rows.filter((x) => x.warning)" :key="r.name + '-warn'" class="warn-item">
        <span class="warn-icon">⚠</span>
        <span class="warn-driver">{{ r.name }}</span>
        <span class="warn-msg">{{ r.warning }}</span>
      </div>
    </div>

    <!-- Butler fallback 提示 -->
    <div v-if="fallbackActive" class="fallback-notice">
      <span class="fb-icon">ℹ</span>
      <span class="fb-text">
        当前驱动不支持管家模式，设备管家实际使用：<strong>{{ butlerDriver }}</strong>
      </span>
    </div>
  </div>

  <div v-if="note" class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</div>
</template>

<style scoped>
/* Driver 网格布局 — 响应式，最小宽度 280px */
.driver-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 12px;
  margin: 14px 0;
}

/* Driver 卡片 — 从简单 chip 升级为信息卡片 */
.driver-card {
  display: flex;
  flex-direction: column;
  gap: 8px;
  text-align: left;
  font: inherit;
  cursor: pointer;
  background: rgba(7, 11, 14, 0.5);
  border: 1px solid var(--ghost);
  border-radius: 9px;
  padding: 12px 13px;
  color: var(--lit);
  transition: all 0.15s ease;
}

.driver-card:hover:not(:disabled) {
  border-color: var(--dim);
  background: rgba(13, 18, 22, 0.72);
}

.driver-card.active {
  border-color: var(--accent);
  background: rgba(46, 196, 160, 0.10);
  box-shadow: 0 0 0 1px rgba(46, 196, 160, 0.25);
}

.driver-card.missing {
  opacity: 0.45;
  cursor: not-allowed;
}

.driver-card.has-warning {
  border-left: 3px solid var(--err);
}

.driver-card:disabled {
  cursor: not-allowed;
}

/* Header: 名称 + 徽章 */
.dc-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px;
}

.dc-name {
  font-size: 14px;
  font-weight: 600;
  color: var(--lit);
  letter-spacing: 0.02em;
}

.dc-badges {
  display: flex;
  gap: 5px;
  flex-wrap: wrap;
}

.dc-badge {
  font-size: 9px;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  border: 1px solid var(--ghost);
  border-radius: 4px;
  padding: 2px 6px;
  color: var(--dim);
  font-weight: 600;
  white-space: nowrap;
}

.dc-badge.current {
  color: #04110f;
  background: var(--accent);
  border-color: var(--accent);
}

.dc-badge.ok {
  color: var(--accent);
  border-color: #1f4d47;
  background: rgba(46, 196, 160, 0.08);
}

.dc-badge.missing {
  color: var(--dim);
  border-color: var(--ghost);
}

.dc-badge.info {
  color: #6e8a93;
  border-color: #203038;
}

.dc-badge.warn {
  color: var(--err);
  border-color: #3a2422;
  background: rgba(230, 111, 111, 0.08);
}

/* Description + Model */
.dc-desc {
  font-size: 11px;
  color: var(--dim);
  line-height: 1.5;
}

.dc-model {
  color: var(--accent);
  font-weight: 500;
  margin-left: 4px;
}

/* Capabilities 行 */
.dc-caps {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  padding-top: 6px;
  border-top: 1px dotted rgba(110, 138, 147, 0.3);
}

.dc-cap {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: 10px;
  letter-spacing: 0.04em;
  padding: 2px 7px;
  border-radius: 4px;
  border: 1px solid var(--ghost);
  background: rgba(7, 11, 14, 0.4);
}

.dc-cap.on {
  color: var(--accent);
  border-color: #1f4d47;
  background: rgba(46, 196, 160, 0.06);
}

.dc-cap.off {
  color: var(--dim);
  opacity: 0.6;
}

.dc-cap-icon {
  font-size: 9px;
}

.dc-cap-label {
  font-weight: 500;
}

/* 空状态 */
.empty-state {
  grid-column: 1 / -1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 32px 16px;
  color: var(--dim);
}

.empty-icon {
  font-size: 32px;
  opacity: 0.4;
}

.empty-text {
  font-size: 12px;
  letter-spacing: 0.04em;
}

/* Warning 详情面板 */
.warnings-panel {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
  padding: 11px 13px;
  background: rgba(230, 111, 111, 0.08);
  border: 1px solid #3a2422;
  border-radius: 8px;
}

.warn-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  font-size: 11px;
  line-height: 1.5;
}

.warn-icon {
  flex: none;
  color: var(--err);
  font-size: 13px;
}

.warn-driver {
  flex: none;
  font-weight: 600;
  color: var(--lit);
  min-width: 80px;
}

.warn-msg {
  flex: 1;
  color: var(--dim);
}

/* Butler fallback 通知 */
.fallback-notice {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  margin-top: 12px;
  padding: 10px 12px;
  background: rgba(46, 196, 160, 0.08);
  border: 1px solid #1f4d47;
  border-radius: 8px;
  font-size: 11px;
  line-height: 1.6;
  color: var(--dim);
}

.fb-icon {
  flex: none;
  color: var(--accent);
  font-size: 14px;
}

.fb-text strong {
  color: var(--accent);
  font-weight: 600;
}

/* 响应式调整 */
@media (max-width: 640px) {
  .driver-grid {
    grid-template-columns: 1fr;
  }
}
</style>
