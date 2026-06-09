<script setup lang="ts">
import { onMounted, ref } from "vue";
import { workspaceFiles, workspaceFile, type WorkspaceFileMeta } from "../api";

const files = ref<WorkspaceFileMeta[]>([]);
const active = ref<string | null>(null);
const content = ref("");
const loading = ref(false);

async function load() {
  try { files.value = await workspaceFiles(); } catch { files.value = []; }
}
async function open(name: string) {
  active.value = name; loading.value = true; content.value = "";
  try {
    const d = await workspaceFile(name);
    content.value = d.exists ? (d.content + (d.truncated ? "\n\n… (已截断)" : "") || "（空文件）") : "（文件还不存在）";
  } catch (e: any) { content.value = "读取失败：" + e.message; }
  finally { loading.value = false; }
}

onMounted(load);
</script>

<template>
  <div class="card">
    <h2>管家工作区文件（只读预览）</h2>
    <p class="hint">管家的人设（CLAUDE.md）与按需读取的长期记忆。预热扫描的项目摘要写在 MEMORY/projects.md。</p>
    <div class="files">
      <span v-if="!files.length" class="empty">加载中…</span>
      <button v-for="f in files" :key="f.name" class="chip"
        :class="{ active: active === f.name, missing: !f.exists }" @click="open(f.name)">
        {{ f.name }}<span class="sz">{{ f.exists ? f.size + 'B' : '缺失' }}</span>
      </button>
    </div>
    <pre class="preview"><span v-if="!active" class="ph">选择上方文件查看内容。</span><span v-else-if="loading" class="ph">加载中…</span><template v-else>{{ content }}</template></pre>
  </div>
</template>
