<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import { workspaceFiles, workspaceFile, type WorkspaceFileMeta } from "../api";

interface Entry { meta: WorkspaceFileMeta; content: string; loading: boolean }

const REFRESH_MS = 5000;

const entries = ref<Entry[]>([]);
const loading = ref(false);
let timer: ReturnType<typeof setInterval> | null = null;

async function fill(entry: Entry) {
  entry.loading = true;
  try {
    const d = await workspaceFile(entry.meta.name);
    entry.content = d.exists
      ? ((d.content + (d.truncated ? "\n\n… (已截断)" : "")) || "（空文件）")
      : "（文件还不存在）";
  } catch (e: any) {
    entry.content = "读取失败：" + e.message;
  } finally {
    entry.loading = false;
  }
}

async function load() {
  loading.value = true;
  try {
    const files = await workspaceFiles();
    entries.value = files.map((meta) => ({ meta, content: "", loading: true }));
    await Promise.all(entries.value.map(fill));
  } catch {
    entries.value = [];
  } finally {
    loading.value = false;
  }
}

onMounted(() => {
  load();
  timer = setInterval(load, REFRESH_MS);
});

onUnmounted(() => {
  if (timer !== null) {
    clearInterval(timer);
    timer = null;
  }
});
</script>

<template>
  <div class="card">
    <h2>管家工作区文件（只读预览）</h2>
    <p class="hint">管家的人设（CLAUDE.md）与按需读取的长期记忆。预热扫描的项目摘要写在 MEMORY/projects.md。</p>
    <span v-if="loading && !entries.length" class="ph">加载中…</span>
    <span v-else-if="!entries.length" class="ph">没有可显示的文件。</span>
    <div v-else class="wsfiles">
      <section v-for="e in entries" :key="e.meta.name" class="wsfile" :class="{ missing: !e.meta.exists }">
        <header class="wsfile-head">
          <span class="wsfile-name">{{ e.meta.name }}</span>
          <span class="wsfile-meta">{{ e.meta.exists ? e.meta.size + 'B' : '缺失' }}</span>
        </header>
        <pre class="preview"><span v-if="e.loading" class="ph">加载中…</span><template v-else>{{ e.content }}</template></pre>
      </section>
    </div>
  </div>
</template>

<style scoped>
.wsfiles { display: grid; gap: 16px; }
.wsfile-head { display: flex; align-items: baseline; gap: 10px; margin-bottom: 7px; }
.wsfile-name { font-size: 13px; font-weight: 600; color: var(--lit); }
.wsfile-meta { font-size: 10px; color: var(--dim); }
.wsfile.missing .wsfile-name { color: var(--dim); }
.wsfile.missing { opacity: .6; }
</style>
