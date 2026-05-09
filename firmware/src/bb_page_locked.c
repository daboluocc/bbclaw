/**
 * LOCKED page — independent full-screen padlock view.
 * Shown when the device is locked (cloud_saas passphrase unlock).
 */
#include "bb_page_locked.h"

#include <stdio.h>
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

/* Large battery widget (below padlock) */
static lv_obj_t* s_bat_container;
static lv_obj_t* s_bat_frame;
static lv_obj_t* s_bat_fill;
static lv_obj_t* s_bat_cap;
static lv_obj_t* s_bat_charge_lbl;
static lv_obj_t* s_bat_pct_lbl;

/* Large battery dimensions */
#define BAT_LG_W         60
#define BAT_LG_H         24
#define BAT_LG_FRAME_W   56
#define BAT_LG_FRAME_H   24
#define BAT_LG_FILL_W    50
#define BAT_LG_FILL_H    18
#define BAT_LG_CAP_W      4
#define BAT_LG_CAP_H     12

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

  /* Title — shifted down to make room for battery widget */
  s_lbl_title = lv_label_create(s_view);
  lv_obj_set_width(s_lbl_title, body_w);
  lv_obj_set_style_text_color(s_lbl_title, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_title, font, 0);
  lv_obj_set_style_text_align(s_lbl_title, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_title, "设备已锁定");
  lv_obj_set_pos(s_lbl_title, UI_SAFE_LEFT, 208);

  /* Hint — shifted down */
  s_lbl_hint = lv_label_create(s_view);
  lv_obj_set_width(s_lbl_hint, body_w);
  lv_obj_set_style_text_color(s_lbl_hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_lbl_hint, font, 0);
  lv_obj_set_style_text_align(s_lbl_hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_hint, "请按住说话键后说出密语");
  lv_obj_set_pos(s_lbl_hint, UI_SAFE_LEFT, 230);

  /* Large battery widget — centered below padlock (padlock body bottom ≈ y=104) */
  {
    const int bat_x = (DISP_W - BAT_LG_W) / 2;
    const int bat_y = 116; /* just below padlock body */

    s_bat_container = lv_obj_create(s_view);
    lv_obj_remove_style_all(s_bat_container);
    lv_obj_set_size(s_bat_container, BAT_LG_W, BAT_LG_H);
    lv_obj_set_pos(s_bat_container, bat_x, bat_y);
    lv_obj_clear_flag(s_bat_container, LV_OBJ_FLAG_SCROLLABLE);

    /* Fill bar */
    s_bat_fill = lv_obj_create(s_bat_container);
    lv_obj_remove_style_all(s_bat_fill);
    lv_obj_set_size(s_bat_fill, BAT_LG_FILL_W, BAT_LG_FILL_H);
    lv_obj_set_pos(s_bat_fill, 3, 3);
    lv_obj_set_style_radius(s_bat_fill, 2, 0);
    lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_bat_fill, LV_OPA_COVER, 0);

    /* Outer frame */
    s_bat_frame = lv_obj_create(s_bat_container);
    lv_obj_remove_style_all(s_bat_frame);
    lv_obj_set_size(s_bat_frame, BAT_LG_FRAME_W, BAT_LG_FRAME_H);
    lv_obj_set_pos(s_bat_frame, 0, 0);
    lv_obj_set_style_radius(s_bat_frame, 4, 0);
    lv_obj_set_style_border_width(s_bat_frame, 2, 0);
    lv_obj_set_style_border_color(s_bat_frame, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_border_opa(s_bat_frame, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_opa(s_bat_frame, LV_OPA_0, 0);
    lv_obj_clear_flag(s_bat_frame, LV_OBJ_FLAG_SCROLLABLE);

    /* Positive terminal cap */
    s_bat_cap = lv_obj_create(s_bat_container);
    lv_obj_remove_style_all(s_bat_cap);
    lv_obj_set_size(s_bat_cap, BAT_LG_CAP_W, BAT_LG_CAP_H);
    lv_obj_set_pos(s_bat_cap, BAT_LG_FRAME_W, (BAT_LG_FRAME_H - BAT_LG_CAP_H) / 2);
    lv_obj_set_style_radius(s_bat_cap, 1, 0);
    lv_obj_set_style_bg_color(s_bat_cap, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_bat_cap, LV_OPA_COVER, 0);

    /* Charging lightning overlay */
    s_bat_charge_lbl = lv_label_create(s_bat_container);
    lv_obj_set_style_text_color(s_bat_charge_lbl, lv_color_hex(0x0a0e0c), 0);
    lv_obj_set_style_text_font(s_bat_charge_lbl, font, 0);
    lv_label_set_text(s_bat_charge_lbl, LV_SYMBOL_CHARGE);
    lv_obj_center(s_bat_charge_lbl);
    lv_obj_add_flag(s_bat_charge_lbl, LV_OBJ_FLAG_HIDDEN);

    /* Percent label below battery */
    s_bat_pct_lbl = lv_label_create(s_view);
    lv_obj_set_width(s_bat_pct_lbl, BAT_LG_W + 20);
    lv_obj_set_style_text_color(s_bat_pct_lbl, lv_color_hex(UI_TEXT_MAIN), 0);
    lv_obj_set_style_text_font(s_bat_pct_lbl, font, 0);
    lv_obj_set_style_text_align(s_bat_pct_lbl, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_bat_pct_lbl, "");
    lv_obj_set_pos(s_bat_pct_lbl, bat_x - 10, bat_y + BAT_LG_H + 4);

    /* Hidden by default until battery data arrives */
    lv_obj_add_flag(s_bat_container, LV_OBJ_FLAG_HIDDEN);
    lv_obj_add_flag(s_bat_pct_lbl, LV_OBJ_FLAG_HIDDEN);
  }
}

void bb_page_locked_set_visible(int visible) {
  if (s_view == NULL) return;
  if (visible) lv_obj_clear_flag(s_view, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(s_view, LV_OBJ_FLAG_HIDDEN);
}

void bb_page_locked_update_battery(int supported, int available, int percent, int low, int charging) {
  if (s_bat_container == NULL || s_bat_fill == NULL) return;

  if (!supported || !available || percent < 0) {
    lv_obj_add_flag(s_bat_container, LV_OBJ_FLAG_HIDDEN);
    if (s_bat_pct_lbl != NULL) lv_obj_add_flag(s_bat_pct_lbl, LV_OBJ_FLAG_HIDDEN);
    return;
  }

  lv_obj_clear_flag(s_bat_container, LV_OBJ_FLAG_HIDDEN);
  if (s_bat_pct_lbl != NULL) lv_obj_clear_flag(s_bat_pct_lbl, LV_OBJ_FLAG_HIDDEN);

  if (percent > 100) percent = 100;
  if (percent < 0) percent = 0;

  /* Choose color */
  uint32_t fill_color;
  if (charging) {
    fill_color = 0x4cd964;
  } else if (low) {
    fill_color = 0xe66f6f;
  } else {
    fill_color = UI_ME_ACCENT;
  }

  int fill_w = charging ? BAT_LG_FILL_W : (percent * BAT_LG_FILL_W) / 100;
  if (fill_w < 1 && percent > 0) fill_w = 1;

  lv_obj_set_width(s_bat_fill, fill_w);
  lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(fill_color), 0);

  if (s_bat_frame != NULL) {
    lv_obj_set_style_border_color(s_bat_frame,
                                  lv_color_hex(charging ? 0x4cd964 : (low ? 0xe66f6f : UI_ME_ACCENT)), 0);
  }
  if (s_bat_cap != NULL) {
    lv_obj_set_style_bg_color(s_bat_cap,
                               lv_color_hex(charging ? 0x4cd964 : (low ? 0xe66f6f : UI_ME_ACCENT)), 0);
  }

  if (s_bat_charge_lbl != NULL) {
    if (charging) lv_obj_clear_flag(s_bat_charge_lbl, LV_OBJ_FLAG_HIDDEN);
    else lv_obj_add_flag(s_bat_charge_lbl, LV_OBJ_FLAG_HIDDEN);
  }

  if (s_bat_pct_lbl != NULL) {
    char buf[16];
    snprintf(buf, sizeof(buf), "%d%%", percent);
    lv_label_set_text(s_bat_pct_lbl, buf);
    lv_obj_set_style_text_color(s_bat_pct_lbl, lv_color_hex(fill_color), 0);
  }
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
