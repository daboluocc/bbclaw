<script setup lang="ts">
import { onMounted, ref } from "vue";
import { health, drivers } from "../api";

const healthLabel = ref("…");
const healthOn = ref(false);
const localLabel = ref("…");
const localOn = ref(false);
const driverList = ref("…");

onMounted(async () => {
  const [h, d] = await Promise.all([health(), drivers()]);
  const st = h?.status ?? "—";
  healthLabel.value = st === "ok" ? "ok" : st;
  healthOn.value = st === "ok";
  const local = h?.local ?? {};
  localOn.value = !!local.ready;
  localLabel.value = local.ready ? "ready" : local.enabled ? "starting" : "off";
  driverList.value = d.drivers.map((x) => x.name).join(", ") || "—";
});
</script>

<template>
  <div class="card">
    <h2>运行状态</h2>
    <div class="status-grid">
      <div class="item"><div class="k">健康</div><div class="v" :class="{ on: healthOn }">{{ healthLabel }}</div></div>
      <div class="item"><div class="k">本地服务</div><div class="v" :class="{ on: localOn }">{{ localLabel }}</div></div>
      <div class="item"><div class="k">已注册驱动</div><div class="v">{{ driverList }}</div></div>
    </div>
  </div>
</template>
