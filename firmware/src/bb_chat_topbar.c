/**
 * CHAT top bar (Phase 3 skeleton).
 * Real implementation is currently in bb_lvgl_display.c's create_ui().
 * During Phase 7 the internal LVGL object creation moves here.
 */
#include "bb_chat_topbar.h"

void bb_chat_topbar_create(lv_obj_t* parent) {
  (void)parent;
  /* Phase 3: no-op */
}

void bb_chat_topbar_set_status(const char* status) {
  (void)status;
}

void bb_chat_topbar_set_clock(const char* hm) {
  (void)hm;
}

void bb_chat_topbar_set_cloud_mode(int is_cloud) {
  (void)is_cloud;
}
