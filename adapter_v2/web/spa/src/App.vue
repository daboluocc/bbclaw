<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from "vue";
import AppHeader from "./components/AppHeader.vue";
import TerminalView from "./views/TerminalView.vue";
import SessionsView from "./views/SessionsView.vue";
import RemindersView from "./views/RemindersView.vue";

type View = "terminal" | "sessions" | "reminders";

// Tiny hash router — keeps deps minimal (no vue-router). "#sessions" → Sessions,
// "#reminders" → Reminders, anything else → Terminal. /admin is a normal anchor
// (full-page nav), so it never reaches this switch.
function viewFromHash(): View {
  const h = location.hash.replace(/^#\/?/, "");
  if (h === "sessions") return "sessions";
  if (h === "reminders") return "reminders";
  return "terminal";
}

const current = ref<View>(viewFromHash());

function onHashChange() {
  current.value = viewFromHash();
}

function nav(v: View) {
  location.hash = v === "terminal" ? "#terminal" : `#${v}`;
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
    <SessionsView v-else-if="current === 'sessions'" :key="activeKey" />
    <RemindersView v-else :key="activeKey" />
  </main>
</template>
