/**
 * STANDBY page — independent full-screen view.
 *
 * Centered three-row composition (320×172):
 *   row 1 (top):    "BBClaw" brand          font_40, accent
 *   row 2 (middle): "12:34" hero clock       font_48, main text
 *   row 3 (bottom): big battery + percent    font_14, color follows state
 *
 * All three rows are horizontally centered to preserve visual symmetry.
 */
#include "bb_page_standby.h"

#include <stdio.h>

#include "bb_config.h"
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

#if LV_FONT_MONTSERRAT_14
LV_FONT_DECLARE(lv_font_montserrat_14)
#endif
#if LV_FONT_MONTSERRAT_48
LV_FONT_DECLARE(lv_font_montserrat_48)
#endif

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

#define UI_SCR_BG       0x0a0e0c
#define UI_TEXT_MAIN    0xd8ebe4
#define UI_TEXT_DIM     0x7a9a8c
#define UI_ME_ACCENT    0x2ec4a0

/* Big battery widget used in row 3 — visibly larger than the top-bar one. */
#define BAT_FRAME_W    96
#define BAT_FRAME_H    32
#define BAT_CAP_W       6
#define BAT_CAP_H      16
#define BAT_W          (BAT_FRAME_W + BAT_CAP_W)
#define BAT_H          BAT_FRAME_H
#define BAT_FILL_INSET  4
#define BAT_FILL_W     (BAT_FRAME_W - 2 * BAT_FILL_INSET)
#define BAT_FILL_H     (BAT_FRAME_H - 2 * BAT_FILL_INSET)
#define BAT_PCT_W      64
#define BAT_GAP        10

#define ROW_BRAND_Y    14
#define ROW_CLOCK_Y    52
#define ROW_BAT_Y     132

static lv_obj_t* s_view;
static lv_obj_t* s_lbl_brand;
static lv_obj_t* s_lbl_clock;

static lv_obj_t* s_bat_container;
static lv_obj_t* s_bat_frame;
static lv_obj_t* s_bat_fill;
static lv_obj_t* s_bat_cap;
static lv_obj_t* s_bat_charge_lbl;
static lv_obj_t* s_bat_pct_lbl;

static const lv_font_t* clock_font_fn(void) {
#if LV_FONT_MONTSERRAT_48
  return &lv_font_montserrat_48;
#else
  return lv_font_get_default();
#endif
}

static const lv_font_t* small_font_fn(void) {
#if LV_FONT_MONTSERRAT_14
  return &lv_font_montserrat_14;
#else
  return lv_font_get_default();
#endif
}

void bb_page_standby_create(lv_obj_t* scr) {
  if (scr == NULL || s_view != NULL) return;

  s_view = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view);
  lv_obj_set_size(s_view, DISP_W, DISP_H);
  lv_obj_set_pos(s_view, 0, 0);
  lv_obj_set_style_bg_color(s_view, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_view, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_view, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view, LV_SCROLLBAR_MODE_OFF);

  const lv_font_t* clock_font = clock_font_fn();
  const lv_font_t* small_font = small_font_fn();

  /* Row 1 — project URL, centered (small font keeps it on one line) */
  s_lbl_brand = lv_label_create(s_view);
  lv_obj_set_style_text_color(s_lbl_brand, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_text_font(s_lbl_brand, small_font, 0);
  lv_obj_set_style_text_align(s_lbl_brand, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_lbl_brand, DISP_W);
  lv_label_set_text(s_lbl_brand, "https://bbclaw.daboluo.cc");
  lv_obj_set_pos(s_lbl_brand, 0, ROW_BRAND_Y);

  /* Row 2 — hero clock, centered */
  s_lbl_clock = lv_label_create(s_view);
  lv_obj_set_style_text_color(s_lbl_clock, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_clock, clock_font, 0);
  lv_obj_set_style_text_align(s_lbl_clock, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_lbl_clock, DISP_W);
  lv_label_set_text(s_lbl_clock, "--:--");
  lv_obj_set_pos(s_lbl_clock, 0, ROW_CLOCK_Y);

  /* Row 3 — battery + percent, horizontally centered as a row.
   * Row total width = BAT_W + BAT_GAP + BAT_PCT_W. */
  const int row_w = BAT_W + BAT_GAP + BAT_PCT_W;
  const int bat_x = (DISP_W - row_w) / 2;
  const int pct_x = bat_x + BAT_W + BAT_GAP;
  const int small_lh = (int)lv_font_get_line_height(small_font);
  const int pct_y = ROW_BAT_Y + (BAT_H - small_lh) / 2;

  s_bat_container = lv_obj_create(s_view);
  lv_obj_remove_style_all(s_bat_container);
  lv_obj_set_size(s_bat_container, BAT_W, BAT_H);
  lv_obj_set_pos(s_bat_container, bat_x, ROW_BAT_Y);
  lv_obj_clear_flag(s_bat_container, LV_OBJ_FLAG_SCROLLABLE);

  /* Fill bar (drawn first so frame border sits on top) */
  s_bat_fill = lv_obj_create(s_bat_container);
  lv_obj_remove_style_all(s_bat_fill);
  lv_obj_set_size(s_bat_fill, BAT_FILL_W, BAT_FILL_H);
  lv_obj_set_pos(s_bat_fill, BAT_FILL_INSET, BAT_FILL_INSET);
  lv_obj_set_style_radius(s_bat_fill, 2, 0);
  lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_bat_fill, LV_OPA_COVER, 0);

  /* Outer frame */
  s_bat_frame = lv_obj_create(s_bat_container);
  lv_obj_remove_style_all(s_bat_frame);
  lv_obj_set_size(s_bat_frame, BAT_FRAME_W, BAT_FRAME_H);
  lv_obj_set_pos(s_bat_frame, 0, 0);
  lv_obj_set_style_radius(s_bat_frame, 5, 0);
  lv_obj_set_style_border_width(s_bat_frame, 3, 0);
  lv_obj_set_style_border_color(s_bat_frame, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_border_opa(s_bat_frame, LV_OPA_COVER, 0);
  lv_obj_set_style_bg_opa(s_bat_frame, LV_OPA_0, 0);
  lv_obj_clear_flag(s_bat_frame, LV_OBJ_FLAG_SCROLLABLE);

  /* Positive terminal cap */
  s_bat_cap = lv_obj_create(s_bat_container);
  lv_obj_remove_style_all(s_bat_cap);
  lv_obj_set_size(s_bat_cap, BAT_CAP_W, BAT_CAP_H);
  lv_obj_set_pos(s_bat_cap, BAT_FRAME_W, (BAT_FRAME_H - BAT_CAP_H) / 2);
  lv_obj_set_style_radius(s_bat_cap, 2, 0);
  lv_obj_set_style_bg_color(s_bat_cap, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_bat_cap, LV_OPA_COVER, 0);

  /* Charging lightning overlay — drawn dark on bright fill for legibility */
  s_bat_charge_lbl = lv_label_create(s_bat_container);
  lv_obj_set_style_text_color(s_bat_charge_lbl, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_text_font(s_bat_charge_lbl, small_font, 0);
  lv_label_set_text(s_bat_charge_lbl, LV_SYMBOL_CHARGE);
  lv_obj_center(s_bat_charge_lbl);
  lv_obj_add_flag(s_bat_charge_lbl, LV_OBJ_FLAG_HIDDEN);

  /* Percent label — sits to the right of the battery, vertically aligned */
  s_bat_pct_lbl = lv_label_create(s_view);
  lv_obj_set_width(s_bat_pct_lbl, BAT_PCT_W);
  lv_obj_set_style_text_color(s_bat_pct_lbl, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_bat_pct_lbl, small_font, 0);
  lv_obj_set_style_text_align(s_bat_pct_lbl, LV_TEXT_ALIGN_LEFT, 0);
  lv_label_set_text(s_bat_pct_lbl, "");
  lv_obj_set_pos(s_bat_pct_lbl, pct_x, pct_y);

  /* Hidden until update_battery() delivers valid data */
  lv_obj_add_flag(s_bat_container, LV_OBJ_FLAG_HIDDEN);
  lv_obj_add_flag(s_bat_pct_lbl, LV_OBJ_FLAG_HIDDEN);
}

void bb_page_standby_set_visible(int visible) {
  if (s_view == NULL) return;
  if (visible) lv_obj_clear_flag(s_view, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(s_view, LV_OBJ_FLAG_HIDDEN);
}

void bb_page_standby_refresh_clock(const char* hm) {
  if (s_lbl_clock == NULL || hm == NULL) return;
  lv_label_set_text(s_lbl_clock, hm);
}

void bb_page_standby_update_battery(int supported, int available, int percent, int low, int charging) {
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

  uint32_t fill_color;
  if (charging) {
    fill_color = 0x4cd964;
  } else if (low) {
    fill_color = 0xe66f6f;
  } else {
    fill_color = UI_ME_ACCENT;
  }

  int fill_w = charging ? BAT_FILL_W : (percent * BAT_FILL_W) / 100;
  if (fill_w < 1 && percent > 0) fill_w = 1;

  lv_obj_set_width(s_bat_fill, fill_w);
  lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(fill_color), 0);

  if (s_bat_frame != NULL) {
    lv_obj_set_style_border_color(s_bat_frame, lv_color_hex(fill_color), 0);
  }
  if (s_bat_cap != NULL) {
    lv_obj_set_style_bg_color(s_bat_cap, lv_color_hex(fill_color), 0);
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
