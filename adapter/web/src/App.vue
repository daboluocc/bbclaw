<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import StatusBar from "./components/StatusBar.vue";
import ProjectsPanel from "./components/ProjectsPanel.vue";
import FilesPanel from "./components/FilesPanel.vue";
import Conversation from "./components/Conversation.vue";

type Tab = "manage" | "convo";

function tabFromPath(path: string): Tab {
  return path.replace(/\/+$/, "").endsWith("/conversations") ? "convo" : "manage";
}

function pathForTab(next: Tab) {
  return next === "convo" ? "/admin/conversations" : "/admin/projects";
}

const tab = ref<Tab>(tabFromPath(window.location.pathname));

function setTab(next: Tab) {
  tab.value = next;
  const path = pathForTab(next);
  if (window.location.pathname !== path) window.history.pushState({}, "", path);
}

function syncFromHistory() {
  tab.value = tabFromPath(window.location.pathname);
}

onMounted(() => window.addEventListener("popstate", syncFromHistory));
onUnmounted(() => window.removeEventListener("popstate", syncFromHistory));
</script>

<template>
  <header class="hdr">
    <span class="wordmark">BBCLAW</span>
    <span class="sub">Adapter · 本地管理 · localhost only</span>
    <nav class="tabs">
      <button class="tab" :class="{ on: tab === 'manage' }" @click="setTab('manage')">项目 / 工作区</button>
      <button class="tab" :class="{ on: tab === 'convo' }" @click="setTab('convo')">对话记录</button>
    </nav>
  </header>

  <main>
    <template v-if="tab === 'manage'">
      <StatusBar />
      <ProjectsPanel />
      <FilesPanel />
    </template>
    <Conversation v-else />
  </main>
</template>
