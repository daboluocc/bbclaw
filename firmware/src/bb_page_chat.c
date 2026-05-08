/**
 * CHAT page — top bar + body + bottom bar.
 *
 * Phase 3 skeleton: empty stubs. Real implementation migrated from
 * bb_lvgl_display.c during Phase 4-7. While the stubs exist, the
 * existing s_view_active in bb_lvgl_display.c remains the active view.
 */
#include "bb_page_chat.h"

#include "lvgl.h"

static lv_obj_t* s_view_chat;
static lv_obj_t* s_body;

void bb_page_chat_create(lv_obj_t* scr) {
  (void)scr;
  /* Phase 3: no-op. s_view_active in bb_lvgl_display.c is still the CHAT view. */
}

void bb_page_chat_set_visible(int visible) {
  if (s_view_chat == NULL) return;
  if (visible) lv_obj_clear_flag(s_view_chat, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(s_view_chat, LV_OBJ_FLAG_HIDDEN);
}

lv_obj_t* bb_page_chat_get_body(void) {
  return s_body;
}
