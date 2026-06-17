<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { drivers, setActiveDriver, type DriverRow } from "../api";

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
}

function installed(r: DriverRow): boolean { return r.installed !== false; }

async function pick(r: DriverRow) {
  if (busy.value || !installed(r) || r.name === activeDriver.value) return;
  busy.value = true;
  try {
    await setActiveDriver(r.name);
    setNote(`已切到 ${r.name}，下一轮对话生效（切换会重置对话连续性）`, false);
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
    <div class="files">
      <button
        v-for="r in rows" :key="r.name"
        class="chip"
        :class="{ active: r.name === activeDriver, missing: !installed(r) }"
        :disabled="busy || !installed(r)"
        @click="pick(r)"
      >
        {{ r.name }}
        <span v-if="!installed(r)" class="sz">未安装</span>
        <span v-else-if="r.name === activeDriver" class="sz">当前</span>
        <span v-else-if="r.butler_capable === false" class="sz">非管家</span>
      </button>
      <span v-if="!rows.length" class="empty">没有已注册驱动。</span>
    </div>
    <p
      v-for="r in rows.filter((x) => x.warning)" :key="r.name + '-warn'"
      class="hint" style="color:var(--err)"
    >
      ⚠ {{ r.name }}：{{ r.warning }}
    </p>
    <p v-if="fallbackActive" class="hint" style="color:var(--err)">
      ⚠ 当前驱动不支持管家，设备管家实际使用：<b>{{ butlerDriver }}</b>
    </p>
  </div>

  <div class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</div>
</template>
