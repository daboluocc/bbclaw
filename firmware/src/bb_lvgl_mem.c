/**
 * LVGL custom allocator — PSRAM-first (LV_USE_CUSTOM_MALLOC).
 *
 * Why: the dot-matrix UI style means hundreds of small lv_obj allocations
 * (standby clock 140+, boot splash 210+, chat transcript labels, ...).
 * With CLIB malloc + CONFIG_SPIRAM_MALLOC_ALWAYSINTERNAL=16384, every one
 * of those small allocations landed in INTERNAL RAM, fragmenting it until
 * task creation failed at runtime (websocket task needs an 8 KB contiguous
 * internal stack; observed internal_largest=7680 → "Error create websocket
 * task" → voice streaming dead).
 *
 * Fix: route ALL LVGL allocations to PSRAM (8 MB, cache-backed) with an
 * internal-RAM fallback if PSRAM is somehow exhausted. Internal RAM stays
 * reserved for what actually needs it: task stacks, WiFi/I2S DMA buffers.
 * Display flush buffers are NOT affected — esp_lvgl_port allocates those
 * itself with explicit DMA caps.
 *
 * Compiled out (empty TU) unless CONFIG_LV_USE_CUSTOM_MALLOC is selected,
 * so the breadboard/simulator builds with builtin/clib malloc are unharmed.
 */
#include "lvgl.h"

#if LV_USE_STDLIB_MALLOC == LV_STDLIB_CUSTOM

#include "esp_heap_caps.h"

#define BB_LV_PSRAM_CAPS    (MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT)
#define BB_LV_FALLBACK_CAPS (MALLOC_CAP_8BIT)

void lv_mem_init(void) {
  /* heap_caps is ready long before LVGL init — nothing to do. */
}

void lv_mem_deinit(void) {
}

lv_mem_pool_t lv_mem_add_pool(void* mem, size_t bytes) {
  LV_UNUSED(mem);
  LV_UNUSED(bytes);
  return NULL; /* pools not supported on heap_caps */
}

void lv_mem_remove_pool(lv_mem_pool_t pool) {
  LV_UNUSED(pool);
}

void* lv_malloc_core(size_t size) {
  void* p = heap_caps_malloc(size, BB_LV_PSRAM_CAPS);
  if (p == NULL) {
    p = heap_caps_malloc(size, BB_LV_FALLBACK_CAPS);
  }
  return p;
}

void* lv_realloc_core(void* p, size_t new_size) {
  void* np = heap_caps_realloc(p, new_size, BB_LV_PSRAM_CAPS);
  if (np == NULL) {
    np = heap_caps_realloc(p, new_size, BB_LV_FALLBACK_CAPS);
  }
  return np;
}

void lv_free_core(void* p) {
  heap_caps_free(p);
}

void lv_mem_monitor_core(lv_mem_monitor_t* mon_p) {
  if (mon_p == NULL) return;
  lv_memzero(mon_p, sizeof(lv_mem_monitor_t));
  mon_p->total_size = heap_caps_get_total_size(BB_LV_PSRAM_CAPS);
  mon_p->free_size = heap_caps_get_free_size(BB_LV_PSRAM_CAPS);
  mon_p->free_biggest_size = heap_caps_get_largest_free_block(BB_LV_PSRAM_CAPS);
  if (mon_p->total_size > 0) {
    mon_p->used_pct =
        (lv_uintptr_t)(100 - (100ULL * mon_p->free_size) / mon_p->total_size);
  }
}

lv_result_t lv_mem_test_core(void) {
  return LV_RESULT_OK;
}

#endif /* LV_USE_STDLIB_MALLOC == LV_STDLIB_CUSTOM */
