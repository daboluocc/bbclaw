<script setup lang="ts">
import { ref } from "vue";
import StatusBar from "./components/StatusBar.vue";
import ProjectsPanel from "./components/ProjectsPanel.vue";
import FilesPanel from "./components/FilesPanel.vue";
import Conversation from "./components/Conversation.vue";

type Tab = "manage" | "convo";
const tab = ref<Tab>("manage");
</script>

<template>
  <header class="hdr">
    <span class="wordmark">BBCLAW</span>
    <span class="sub">Adapter · 本地管理 · localhost only</span>
    <nav class="tabs">
      <button class="tab" :class="{ on: tab === 'manage' }" @click="tab = 'manage'">项目 / 工作区</button>
      <button class="tab" :class="{ on: tab === 'convo' }" @click="tab = 'convo'">对话记录</button>
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
