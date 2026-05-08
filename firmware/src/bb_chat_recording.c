/**
 * CHAT recording overlay — semi-transparent mic/VU mask (Phase 5 skeleton).
 * Full implementation migrated from bb_lvgl_display.c during Phase 7.
 */
#include "bb_chat_recording.h"

void bb_chat_recording_create(lv_obj_t* parent, int width, int height_px) {
  (void)parent;
  (void)width;
  (void)height_px;
}

void bb_chat_recording_show(void) {}

void bb_chat_recording_hide(void) {}

void bb_chat_recording_set_level(uint8_t level_pct, int voiced) {
  (void)level_pct;
  (void)voiced;
}
