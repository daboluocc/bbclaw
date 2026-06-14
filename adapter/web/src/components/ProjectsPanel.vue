<script setup lang="ts">
import { onMounted, ref } from "vue";
import { addProject, listProjects, removeProject, resolveDroppedDir, type Project } from "../api";
import DirPicker from "./DirPicker.vue";

const projects = ref<Project[]>([]);
const note = ref("");
const noteErr = ref(false);
const picking = ref(false);
const freshName = ref<string | null>(null);
const dragActive = ref(false);
const busy = ref(false);
let dragDepth = 0;

async function load() {
  try { projects.value = await listProjects(); }
  catch (e: any) { setNote("加载失败：" + e.message, true); }
}
function setNote(t: string, err: boolean) { note.value = t; noteErr.value = err; }

async function del(name: string) {
  if (!confirm(`删除项目「${name}」？管家将不再能在该目录派活。`)) return;
  try { await removeProject(name); setNote(`已删除 ${name}`, false); load(); }
  catch (e: any) { setNote("删除失败：" + e.message, true); }
}

function pathFromFileURL(raw: string): string {
  try {
    const u = new URL(raw.trim());
    if (u.protocol !== "file:") return "";
    return decodeURIComponent(u.pathname);
  } catch { return ""; }
}

function collectDroppedPaths(dt: DataTransfer): string[] {
  const out = new Set<string>();
  const add = (p: string) => {
    const v = p.trim();
    if (v.startsWith("/")) out.add(v);
    else if (v.startsWith("file://")) {
      const fp = pathFromFileURL(v);
      if (fp) out.add(fp);
    }
  };

  for (const line of dt.getData("text/uri-list").split(/\r?\n/)) {
    if (line && !line.startsWith("#")) add(line);
  }
  for (const line of dt.getData("text/plain").split(/\r?\n/)) add(line);

  for (const f of Array.from(dt.files)) {
    const p = (f as File & { path?: string }).path || "";
    if (p) add(p);
  }
  return [...out];
}

function collectDroppedNames(dt: DataTransfer): string[] {
  const out = new Set<string>();
  for (const item of Array.from(dt.items || [])) {
    const entry = (item as DataTransferItem & { webkitGetAsEntry?: () => { isDirectory?: boolean; name?: string } | null }).webkitGetAsEntry?.();
    if (entry?.isDirectory && entry.name) out.add(entry.name);
  }
  for (const f of Array.from(dt.files)) {
    if (f.name) out.add(f.name);
  }
  return [...out].filter((name) => !!name && !name.includes("/") && !name.includes("\\") && !name.startsWith("."));
}

async function addPaths(paths: string[]) {
  if (busy.value) return;
  busy.value = true;
  setNote("正在添加目录…", false);
  let ok = 0, fail = 0, last: string | null = null;
  for (const p of paths) {
    try { const proj = await addProject(p); ok++; last = proj.name; } catch { fail++; }
  }
  busy.value = false;
  freshName.value = last;
  setNote(ok ? `已添加 ${ok} 个${fail ? `，${fail} 个失败` : ""}，正在后台预热…` : "没有添加成功，请确认拖入的是本机目录路径。", ok === 0);
  load();
  if (last) setTimeout(() => (freshName.value = null), 1400);
}

async function resolveDroppedNames(names: string[]): Promise<string[]> {
  const paths: string[] = [];
  for (const name of names) {
    try {
      const res = await resolveDroppedDir(name);
      if (res.path) paths.push(res.path);
    } catch { /* ignore unresolved names */ }
  }
  return [...new Set(paths)];
}

function onDragEnter(e: DragEvent) {
  e.preventDefault();
  dragDepth++;
  dragActive.value = true;
}
function onDragOver(e: DragEvent) {
  e.preventDefault();
  if (e.dataTransfer) e.dataTransfer.dropEffect = "copy";
}
function onDragLeave() {
  dragDepth = Math.max(0, dragDepth - 1);
  if (dragDepth === 0) dragActive.value = false;
}
async function onDrop(e: DragEvent) {
  e.preventDefault();
  dragDepth = 0;
  dragActive.value = false;
  let paths = e.dataTransfer ? collectDroppedPaths(e.dataTransfer) : [];
  if (!paths.length && e.dataTransfer) {
    const names = collectDroppedNames(e.dataTransfer);
    if (names.length) {
      setNote("正在解析拖入目录…", false);
      paths = await resolveDroppedNames(names);
    }
  }
  if (!paths.length) {
    picking.value = true;
    setNote("没能定位到拖入目录，已打开目录选择器。可点「外挂盘」进入 /Volumes。", false);
    return;
  }
  await addPaths(paths);
}

function onAdded(count: number, lastName: string | null) {
  picking.value = false;
  freshName.value = lastName;
  setNote(count ? `已添加 ${count} 个，正在后台预热…` : "没有添加（重复/无效）", count === 0);
  load();
  if (lastName) setTimeout(() => (freshName.value = null), 1400);
}

onMounted(load);
</script>

<template>
  <div
    class="card project-card"
    :class="{ dragging: dragActive }"
    @dragenter="onDragEnter"
    @dragover="onDragOver"
    @dragleave="onDragLeave"
    @drop="onDrop"
  >
    <h2>项目目录（管家可派活的白名单）</h2>
    <p class="hint">加入一个目录＝授予管家在其中跑编码任务（含命令/文件执行）的权限。加入后自动轻量扫描仓库、预热进管家长期记忆。名称从目录名自动生成。</p>
    <div class="dropzone" :class="{ on: dragActive }">
      <div class="dz-title">拖入路径添加</div>
      <div class="dz-hint">支持 file://、纯文本绝对路径；Finder 目录会按目录名在 /Volumes 和常用工作区中自动定位。</div>
    </div>
    <table>
      <thead><tr><th>名称</th><th>路径</th><th>来源</th><th></th></tr></thead>
      <tbody>
        <tr v-if="!projects.length"><td colspan="4" class="empty">还没有项目。点下面「选择目录添加」。</td></tr>
        <tr v-for="p in projects" :key="p.name" :class="{ fresh: p.name === freshName }">
          <td>{{ p.name }}</td>
          <td class="path">{{ p.path }}</td>
          <td><span class="tag" :class="{ admin: p.source === 'admin' }">{{ p.source === 'admin' ? '已添加' : '初始导入' }}</span></td>
          <td><button class="del" @click="del(p.name)">删除</button></td>
        </tr>
      </tbody>
    </table>
    <div style="margin-top:14px"><button @click="picking = true">＋ 选择目录添加</button></div>
    <div class="msg" :class="{ err: noteErr, ok: !noteErr }">{{ note }}</div>
  </div>

  <DirPicker v-if="picking" @close="picking = false" @added="onAdded" />
</template>
