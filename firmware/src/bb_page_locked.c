/**
 * LOCKED page — independent full-screen padlock view.
 * Shown when the device is locked (cloud_saas passphrase unlock).
 */
#include "bb_page_locked.h"

#include <string.h>

#include "bb_config.h"
#include "bb_status.h"
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

#define UI_TEXT_MAIN    0xd8ebe4
#define UI_TEXT_DIM     0x7a9a8c
#define UI_ME_ACCENT    0x2ec4a0
#define UI_SAFE_LEFT    10
#define UI_SAFE_RIGHT   12

static lv_obj_t* s_view;
static lv_obj_t* s_obj_shackle;
static lv_obj_t* s_obj_body;
static lv_obj_t* s_obj_slot;
static lv_obj_t* s_lbl_title;
static lv_obj_t* s_lbl_hint;

static const lv_font_t* ui_font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

void bb_page_locked_create(lv_obj_t* scr) {
  if (scr == NULL || s_view != NULL) return;

  const int body_w = DISP_W - UI_SAFE_LEFT - UI_SAFE_RIGHT;
  const lv_font_t* font = ui_font();

  s_view = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view);
  lv_obj_set_size(s_view, DISP_W, DISP_H);
  lv_obj_set_pos(s_view, 0, 0);
  lv_obj_clear_flag(s_view, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view, LV_SCROLLBAR_MODE_OFF);

  /* Padlock shackle (U-shaped border) */
  s_obj_shackle = lv_obj_create(s_view);
  lv_obj_remove_style_all(s_obj_shackle);
  lv_obj_set_size(s_obj_shackle, 42, 30);
  lv_obj_set_pos(s_obj_shackle, (DISP_W - 42) / 2, 28);
  lv_obj_set_style_radius(s_obj_shackle, 18, 0);
  lv_obj_set_style_border_width(s_obj_shackle, 3, 0);
  lv_obj_set_style_border_color(s_obj_shackle, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_obj_shackle, LV_OPA_0, 0);

  /* Padlock body */
  s_obj_body = lv_obj_create(s_view);
  lv_obj_remove_style_all(s_obj_body);
  lv_obj_set_size(s_obj_body, 60, 52);
  lv_obj_set_pos(s_obj_body, (DISP_W - 60) / 2, 52);
  lv_obj_set_style_radius(s_obj_body, 12, 0);
  lv_obj_set_style_bg_color(s_obj_body, lv_color_hex(0x163128), 0);
  lv_obj_set_style_bg_opa(s_obj_body, LV_OPA_COVER, 0);
  lv_obj_set_style_border_width(s_obj_body, 1, 0);
  lv_obj_set_style_border_color(s_obj_body, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_border_opa(s_obj_body, LV_OPA_70, 0);

  /* Keyhole slot */
  s_obj_slot = lv_obj_create(s_obj_body);
  lv_obj_remove_style_all(s_obj_slot);
  lv_obj_set_size(s_obj_slot, 10, 20);
  lv_obj_set_pos(s_obj_slot, 25, 14);
  lv_obj_set_style_radius(s_obj_slot, 5, 0);
  lv_obj_set_style_bg_color(s_obj_slot, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_obj_slot, LV_OPA_90, 0);

  /* Title */
  s_lbl_title = lv_label_create(s_view);
  lv_obj_set_width(s_lbl_title, body_w);
  lv_obj_set_style_text_color(s_lbl_title, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_title, font, 0);
  lv_obj_set_style_text_align(s_lbl_title, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_title, "设备已锁定");
  lv_obj_set_pos(s_lbl_title, UI_SAFE_LEFT, 118);

  /* Hint */
  s_lbl_hint = lv_label_create(s_view);
  lv_obj_set_width(s_lbl_hint, body_w);
  lv_obj_set_style_text_color(s_lbl_hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_lbl_hint, font, 0);
  lv_obj_set_style_text_align(s_lbl_hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_hint, "请按住说话键后说出密语");
  lv_obj_set_pos(s_lbl_hint, UI_SAFE_LEFT, 140);
}

void bb_page_locked_set_visible(int visible) {
  if (s_view == NULL) return;
  if (visible) lv_obj_clear_flag(s_view, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(s_view, LV_OBJ_FLAG_HIDDEN);
}

void bb_page_locked_update_status(const char* status) {
  if (s_lbl_title == NULL || s_lbl_hint == NULL) return;
  if (status != NULL && strcmp(status, BB_STATUS_VERIFY_TX) == 0) {
    lv_label_set_text(s_lbl_title, "正在聆听密语");
    lv_label_set_text(s_lbl_hint, "松开按键后开始验证");
  } else if (status != NULL && strcmp(status, BB_STATUS_VERIFY) == 0) {
    lv_label_set_text(s_lbl_title, "正在验证密语");
    lv_label_set_text(s_lbl_hint, "请稍候");
  } else if (status != NULL && strcmp(status, BB_STATUS_VERIFY_ERR) == 0) {
    lv_label_set_text(s_lbl_title, "解锁失败");
    lv_label_set_text(s_lbl_hint, "请重新说出密语");
  } else {
    lv_label_set_text(s_lbl_title, "设备已锁定");
    lv_label_set_text(s_lbl_hint, "请按住说话键后说出密语");
  }
}
