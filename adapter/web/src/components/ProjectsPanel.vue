<script setup lang="ts">
import { onMounted, ref } from "vue";
import { listProjects, removeProject, type Project } from "../api";
import DirPicker from "./DirPicker.vue";

const projects = ref<Project[]>([]);
const note = ref("");
const noteErr = ref(false);
const picking = ref(false);
const freshName = ref<string | null>(null);

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
  <div class="card">
    <h2>项目目录（管家可派活的白名单）</h2>
    <p class="hint">加入一个目录＝授予管家在其中跑编码任务（含命令/文件执行）的权限。加入后自动轻量扫描仓库、预热进管家长期记忆。名称从目录名自动生成。</p>
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
