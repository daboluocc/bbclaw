<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { browseDir, searchDir, addProject, type FsEntry } from "../api";

const emit = defineEmits<{ (e: "close"): void; (e: "added", count: number, lastName: string | null): void }>();

const cwd = ref("");
const parentDir = ref("");
const curDirs = ref<FsEntry[]>([]);
const shown = ref<FsEntry[]>([]);
const asSearch = ref(false);
const query = ref("");
const selected = ref(new Map<string, string>());
const note = ref("");
const noteErr = ref(false);
const busy = ref(false);

const selCount = computed(() => selected.value.size);

async function go(path?: string) {
  try {
    const d = await browseDir(path);
    cwd.value = d.path; parentDir.value = d.parent || ""; curDirs.value = d.dirs || [];
    asSearch.value = false; query.value = ""; shown.value = curDirs.value;
  } catch (e: any) { setNote("浏览失败：" + e.message, true); }
}
function applyFilter() {
  const q = query.value.trim().toLowerCase();
  asSearch.value = false;
  shown.value = q ? curDirs.value.filter((d) => d.name.toLowerCase().includes(q)) : curDirs.value;
}
async function recursive() {
  const q = query.value.trim();
  if (!q) return applyFilter();
  setNote("递归搜索中…", false);
  try {
    const d = await searchDir(cwd.value, q);
    shown.value = d.dirs || []; asSearch.value = true;
    setNote(`找到 ${shown.value.length} 个${d.truncated ? "（已截断）" : ""}，点行进入、勾选添加`, false);
  } catch (e: any) { setNote("搜索失败：" + e.message, true); }
}
function toggle(path: string, name: string) {
  if (selected.value.has(path)) selected.value.delete(path); else selected.value.set(path, name);
  selected.value = new Map(selected.value);
}
function isSel(path: string) { return selected.value.has(path); }
function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function addPaths(paths: string[]) {
  busy.value = true;
  let ok = 0, fail = 0, last: string | null = null;
  for (const p of paths) {
    try { const proj = await addProject(p); ok++; last = proj.name; } catch { fail++; }
  }
  busy.value = false;
  emit("added", ok, last);
}

onMounted(() => go());
</script>

<template>
  <div class="overlay" @click.self="emit('close')">
    <div class="modal">
      <h3>选择项目目录（可多选）</h3>
      <div class="toolbar">
        <button class="ghost small" :disabled="!parentDir" @click="parentDir && go(parentDir)">↑ 上级</button>
        <input type="text" v-model="query" placeholder="按关键字过滤 / 回车递归搜索…"
          @input="applyFilter" @keydown.enter.prevent="recursive" />
        <button class="ghost small" @click="recursive">递归搜索</button>
      </div>
      <div class="toolbar"><span class="crumbs">{{ cwd }}</span></div>

      <div class="browser">
        <div v-if="!shown.length" class="empty">
          {{ asSearch ? "没有匹配的目录。" : "（此目录下没有子文件夹，可直接添加它）" }}
        </div>
        <div v-for="d in shown" :key="d.path" class="brow" :class="{ sel: isSel(d.path) }" @click="go(d.path)">
          <input type="checkbox" :checked="isSel(d.path)" @click.stop="toggle(d.path, d.name)" />
          <span class="ic">▸</span>
          <span class="nm">{{ d.name }} <span v-if="asSearch" class="fp">{{ d.path }}</span></span>
        </div>
      </div>

      <div class="actions">
        <span class="selcount">已选 {{ selCount }} 个</span>
        <div class="right">
          <button class="ghost small" @click="emit('close')">取消</button>
          <button class="ghost small" :disabled="!cwd || busy" @click="addPaths([cwd])">添加当前目录</button>
          <button class="small" :disabled="!selCount || busy" @click="addPaths([...selected.keys()])">添加选中 ({{ selCount }})</button>
        </div>
      </div>
      <div class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</div>
    </div>
  </div>
</template>
