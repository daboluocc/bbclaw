<script setup lang="ts">
defineProps<{ current: "terminal" | "sessions" }>();
const emit = defineEmits<{ (e: "nav", v: "terminal" | "sessions"): void }>();
</script>

<template>
  <header class="hdr">
    <button
      class="wordmark"
      title="回到首页 Home"
      @click="emit('nav', 'terminal')"
    >
      BBCLAW
    </button>
    <span class="sub">adapter_v2</span>
    <nav class="tabs">
      <button
        class="tab"
        :class="{ on: current === 'terminal' }"
        @click="emit('nav', 'terminal')"
      >
        终端 Terminal
      </button>
      <button
        class="tab"
        :class="{ on: current === 'sessions' }"
        @click="emit('nav', 'sessions')"
      >
        会话 Sessions
      </button>
    </nav>
    <span class="spacer"></span>
    <!-- /admin is a separate loopback-only page (full-page nav, NOT a SPA route) -->
    <a class="adminlink" href="/admin">⚙ 管理 /admin</a>
  </header>
</template>
