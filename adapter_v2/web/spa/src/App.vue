<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from "vue";
import AppHeader from "./components/AppHeader.vue";
import TerminalView from "./views/TerminalView.vue";
import SessionsView from "./views/SessionsView.vue";

type View = "terminal" | "sessions";

// Tiny hash router — keeps deps minimal (no vue-router). "#sessions" → Sessions,
// anything else → Terminal. /admin is a normal anchor (full-page nav), so it
// never reaches this switch.
function viewFromHash(): View {
  return location.hash.replace(/^#\/?/, "") === "sessions"
    ? "sessions"
    : "terminal";
}

const current = ref<View>(viewFromHash());

function onHashChange() {
  current.value = viewFromHash();
}

function nav(v: View) {
  location.hash = v === "sessions" ? "#sessions" : "#terminal";
  current.value = v;
}

onMounted(() => window.addEventListener("hashchange", onHashChange));
onUnmounted(() => window.removeEventListener("hashchange", onHashChange));

// Keep both views mounted-once but toggle via v-show is wrong for the terminal
// (it needs a live socket only while visible). We mount the active one only,
// keyed so a re-entry remounts the terminal cleanly.
const activeKey = computed(() => current.value);
</script>

<template>
  <AppHeader :current="current" @nav="nav" />
  <main class="view">
    <TerminalView v-if="current === 'terminal'" :key="activeKey" />
    <SessionsView v-else :key="activeKey" />
  </main>
</template>
