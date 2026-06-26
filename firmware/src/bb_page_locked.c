/**
 * LOCKED page — dot-matrix padlock, Nothing-style.
 * Shown when the device is locked (cloud_saas passphrase unlock).
 *
 * Visual language matches bb_page_boot / bb_page_standby (5px dots, theme
 * tokens): a padlock drawn as dots — a 7-dot shackle arc over a 5×4 dot
 * body, with a teal keyhole dot that breathes. The breathing speeds up
 * while a passphrase is being captured/verified, and the body flashes
 * red for the VERIFY_ERR beat. Title/hint live in a block right of the
 * glyph (the old layout put them at y=208/230 — off-screen on the 172px
 * panel). See design/UI_DESIGN_LANGUAGE.md.
 */
#include "bb_page_locked.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_status.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

/* design/UI_DESIGN_LANGUAGE.md tokens */
#define UI_TEXT_MAIN    BB_UI_DOT_LIT
#define UI_TEXT_DIM     BB_UI_TEXT_DIM
#define UI_ME_ACCENT    BB_UI_ACCENT
#define UI_SAFE_RIGHT   12

/* ── dot-matrix padlock geometry ── */
#define MX_DOT        5
#define MX_PITCH      9
#define BODY_COLS     5
#define BODY_ROWS     4
#define SHACKLE_DOTS  7
/* glyph anchor: body top-left dot position */
#define BODY_X        58
#define BODY_Y        70
#define BODY_CX       (BODY_X + 2 * MX_PITCH + MX_DOT / 2) /* keyhole column */
/* keyhole = body dot (col 2, row 1) */
#define KEYHOLE_COL   2
#define KEYHOLE_ROW   1

/* Shackle arc — dot-center offsets from (BODY_CX, BODY_Y - 2), radius 14,
 * semicircle 180°→0° (same offset-table idiom as bb_page_netconn rings). */
typedef struct {
  int8_t dx;
  int8_t dy;
} arc_off_t;
static const arc_off_t SHACKLE[SHACKLE_DOTS] = {
    {-14, 0}, {-12, -7}, {-7, -12}, {0, -14}, {7, -12}, {12, -7}, {14, 0},
};

/* ── right text block ── */
#define TEXT_X 126
#define TEXT_W (DISP_W - TEXT_X - UI_SAFE_RIGHT)

static lv_obj_t* s_view;
static lv_obj_t* s_body_dots[BODY_ROWS][BODY_COLS];
static lv_obj_t* s_shackle_dots[SHACKLE_DOTS];
static lv_obj_t* s_keyhole; /* alias of s_body_dots[KEYHOLE_ROW][KEYHOLE_COL] */
static lv_obj_t* s_lbl_title;
static lv_obj_t* s_lbl_hint;
static int s_body_err;          /* 1 while the body shows the VERIFY_ERR red beat */
static uint32_t s_breathe_ms;   /* current keyhole breathing period */

/* Battery widget — right block, one line: frame+cap, percent to the right */
static lv_obj_t* s_bat_container;
static lv_obj_t* s_bat_frame;
static lv_obj_t* s_bat_fill;
static lv_obj_t* s_bat_cap;
static lv_obj_t* s_bat_charge_lbl;
static lv_obj_t* s_bat_pct_lbl;

#define BAT_LG_FRAME_W   40
#define BAT_LG_FRAME_H   18
#define BAT_LG_FILL_W    34
#define BAT_LG_FILL_H    12
#define BAT_LG_CAP_W      3
#define BAT_LG_CAP_H      8
#define BAT_LG_X         (TEXT_X + 36)
#define BAT_LG_Y         116

static const lv_font_t* ui_font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

static lv_obj_t* make_dot(lv_obj_t* parent, int cx, int cy, uint32_t color) {
  lv_obj_t* d = lv_obj_create(parent);
  lv_obj_remove_style_all(d);
  lv_obj_set_size(d, MX_DOT, MX_DOT);
  lv_obj_set_pos(d, cx - MX_DOT / 2, cy - MX_DOT / 2);
  lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
  lv_obj_set_style_bg_color(d, lv_color_hex(color), 0);
  lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
  lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
  return d;
}

static void keyhole_pulse_cb(void* obj, int32_t v) {
  lv_obj_set_style_bg_opa((lv_obj_t*)obj, (lv_opa_t)v, 0);
}

/* (Re)start the keyhole breathing animation. Faster while verifying.
 * No-op when the period is unchanged — update_status fires on every display
 * refresh, and restarting each time would pin the anim at its start value. */
static void keyhole_breathe(uint32_t period_ms) {
  if (s_keyhole == NULL || s_breathe_ms == period_ms) return;
  s_breathe_ms = period_ms;
  lv_anim_del(s_keyhole, keyhole_pulse_cb);
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, s_keyhole);
  lv_anim_set_values(&a, LV_OPA_30, LV_OPA_COVER);
  lv_anim_set_duration(&a, period_ms);
  lv_anim_set_playback_duration(&a, period_ms);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, keyhole_pulse_cb);
  lv_anim_start(&a);
}

/* Paint every body dot except the keyhole. */
static void set_body_color(uint32_t color) {
  for (int r = 0; r < BODY_ROWS; r++) {
    for (int c = 0; c < BODY_COLS; c++) {
      if (r == KEYHOLE_ROW && c == KEYHOLE_COL) continue;
      if (s_body_dots[r][c] != NULL) {
        lv_obj_set_style_bg_color(s_body_dots[r][c], lv_color_hex(color), 0);
      }
    }
  }
}

void bb_page_locked_create(lv_obj_t* scr) {
  if (scr == NULL || s_view != NULL) return;

  const lv_font_t* font = ui_font();

  s_view = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view);
  lv_obj_set_size(s_view, DISP_W, DISP_H);
  lv_obj_set_pos(s_view, 0, 0);
  lv_obj_set_style_bg_color(s_view, lv_color_hex(BB_UI_BG), 0);
  lv_obj_set_style_bg_opa(s_view, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_view, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view, LV_SCROLLBAR_MODE_OFF);

  /* ── dot-matrix padlock ── */
  for (int i = 0; i < SHACKLE_DOTS; i++) {
    s_shackle_dots[i] = make_dot(s_view, BODY_CX + SHACKLE[i].dx,
                                 BODY_Y - 2 + SHACKLE[i].dy, BB_UI_DOT_LIT);
  }
  for (int r = 0; r < BODY_ROWS; r++) {
    for (int c = 0; c < BODY_COLS; c++) {
      uint32_t color = (r == KEYHOLE_ROW && c == KEYHOLE_COL) ? BB_UI_ACCENT : BB_UI_DOT_LIT;
      s_body_dots[r][c] = make_dot(s_view, BODY_X + c * MX_PITCH + MX_DOT / 2,
                                   BODY_Y + r * MX_PITCH + MX_DOT / 2, color);
    }
  }
  s_keyhole = s_body_dots[KEYHOLE_ROW][KEYHOLE_COL];
  s_body_err = 0;
  s_breathe_ms = 0;
  keyhole_breathe(1100);

  /* ── right text block (on-screen — the old y=208 layout was clipped) ── */
  s_lbl_title = lv_label_create(s_view);
  lv_obj_set_width(s_lbl_title, TEXT_W);
  lv_obj_set_style_text_color(s_lbl_title, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_title, font, 0);
  lv_obj_set_style_text_align(s_lbl_title, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_title, "设备已锁定");
  lv_obj_set_pos(s_lbl_title, TEXT_X, 52);

  s_lbl_hint = lv_label_create(s_view);
  lv_obj_set_width(s_lbl_hint, TEXT_W);
  lv_obj_set_style_text_color(s_lbl_hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_lbl_hint, font, 0);
  lv_obj_set_style_text_align(s_lbl_hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_long_mode(s_lbl_hint, LV_LABEL_LONG_MODE_WRAP);
  lv_label_set_text(s_lbl_hint, "请按住说话键后说出密语");
  lv_obj_set_pos(s_lbl_hint, TEXT_X, 76);

  /* ── battery — one line in the right block: frame+cap, percent right ── */
  {
    s_bat_container = lv_obj_create(s_view);
    lv_obj_remove_style_all(s_bat_container);
    lv_obj_set_size(s_bat_container, BAT_LG_FRAME_W + BAT_LG_CAP_W, BAT_LG_FRAME_H);
    lv_obj_set_pos(s_bat_container, BAT_LG_X, BAT_LG_Y);
    lv_obj_clear_flag(s_bat_container, LV_OBJ_FLAG_SCROLLABLE);

    s_bat_fill = lv_obj_create(s_bat_container);
    lv_obj_remove_style_all(s_bat_fill);
    lv_obj_set_size(s_bat_fill, BAT_LG_FILL_W, BAT_LG_FILL_H);
    lv_obj_set_pos(s_bat_fill, 3, 3);
    lv_obj_set_style_radius(s_bat_fill, 2, 0);
    lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_bat_fill, LV_OPA_COVER, 0);

    s_bat_frame = lv_obj_create(s_bat_container);
    lv_obj_remove_style_all(s_bat_frame);
    lv_obj_set_size(s_bat_frame, BAT_LG_FRAME_W, BAT_LG_FRAME_H);
    lv_obj_set_pos(s_bat_frame, 0, 0);
    lv_obj_set_style_radius(s_bat_frame, 3, 0);
    lv_obj_set_style_border_width(s_bat_frame, 2, 0);
    lv_obj_set_style_border_color(s_bat_frame, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_border_opa(s_bat_frame, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_opa(s_bat_frame, LV_OPA_0, 0);
    lv_obj_clear_flag(s_bat_frame, LV_OBJ_FLAG_SCROLLABLE);

    s_bat_cap = lv_obj_create(s_bat_container);
    lv_obj_remove_style_all(s_bat_cap);
    lv_obj_set_size(s_bat_cap, BAT_LG_CAP_W, BAT_LG_CAP_H);
    lv_obj_set_pos(s_bat_cap, BAT_LG_FRAME_W, (BAT_LG_FRAME_H - BAT_LG_CAP_H) / 2);
    lv_obj_set_style_radius(s_bat_cap, 1, 0);
    lv_obj_set_style_bg_color(s_bat_cap, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_bat_cap, LV_OPA_COVER, 0);

    s_bat_charge_lbl = lv_label_create(s_bat_container);
    lv_obj_set_style_text_color(s_bat_charge_lbl, lv_color_hex(BB_UI_BG), 0);
    lv_obj_set_style_text_font(s_bat_charge_lbl, font, 0);
    lv_label_set_text(s_bat_charge_lbl, LV_SYMBOL_CHARGE);
    lv_obj_center(s_bat_charge_lbl);
    lv_obj_add_flag(s_bat_charge_lbl, LV_OBJ_FLAG_HIDDEN);

    s_bat_pct_lbl = lv_label_create(s_view);
    lv_obj_set_width(s_bat_pct_lbl, 56);
    lv_obj_set_style_text_color(s_bat_pct_lbl, lv_color_hex(UI_TEXT_MAIN), 0);
    lv_obj_set_style_text_font(s_bat_pct_lbl, font, 0);
    lv_obj_set_style_text_align(s_bat_pct_lbl, LV_TEXT_ALIGN_LEFT, 0);
    lv_label_set_text(s_bat_pct_lbl, "");
    lv_obj_set_pos(s_bat_pct_lbl, BAT_LG_X + BAT_LG_FRAME_W + BAT_LG_CAP_W + 8,
                   BAT_LG_Y + (BAT_LG_FRAME_H - (int)lv_font_get_line_height(font)) / 2);

    /* Hidden by default until battery data arrives */
    lv_obj_add_flag(s_bat_container, LV_OBJ_FLAG_HIDDEN);
    lv_obj_add_flag(s_bat_pct_lbl, LV_OBJ_FLAG_HIDDEN);
  }

  /* No bottom status bar on the lock screen — the old "[B] <cwd>" / "mem: N+M"
   * footer was dev/butler info with no meaning to a locked user (removed). */
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

  uint32_t fill_color;
  if (charging) {
    fill_color = BB_UI_OK;
  } else if (low) {
    fill_color = BB_UI_ERR;
  } else {
    fill_color = UI_ME_ACCENT;
  }

  int fill_w = charging ? BAT_LG_FILL_W : (percent * BAT_LG_FILL_W) / 100;
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

void bb_page_locked_update_status(const char* status) {
  if (s_lbl_title == NULL || s_lbl_hint == NULL) return;

  /* Default visuals: calm breathing, cool-white body. */
  uint32_t breathe_ms = 1100;
  int body_err = 0;

  if (status != NULL && strcmp(status, BB_STATUS_VERIFY_TX) == 0) {
    lv_label_set_text(s_lbl_title, "正在聆听密语");
    lv_label_set_text(s_lbl_hint, "松开按键后开始验证");
    breathe_ms = 450; /* capturing — keyhole breathes faster */
  } else if (status != NULL && strcmp(status, BB_STATUS_VERIFY) == 0) {
    lv_label_set_text(s_lbl_title, "正在验证密语");
    lv_label_set_text(s_lbl_hint, "请稍候");
    breathe_ms = 450;
  } else if (status != NULL && strcmp(status, BB_STATUS_VERIFY_ERR) == 0) {
    lv_label_set_text(s_lbl_title, "解锁失败");
    lv_label_set_text(s_lbl_hint, "请重新说出密语");
    body_err = 1; /* red beat — restored on the next status update */
  } else {
    lv_label_set_text(s_lbl_title, "设备已锁定");
    lv_label_set_text(s_lbl_hint, "请按住说话键后说出密语");
  }

  if (body_err != s_body_err) {
    set_body_color(body_err ? BB_UI_ERR : BB_UI_DOT_LIT);
    s_body_err = body_err;
  }
  keyhole_breathe(breathe_ms);
}

void bb_page_locked_show_heard(const char* heard) {
  /* ADR-038: overwrite the hint (just set to "请重新说出密语" by
   * update_status(VERIFY_ERR)) with what the ASR actually heard, so the user can
   * tell why the unlock failed and adjust. Empty → leave the default hint. */
  if (s_lbl_hint == NULL || heard == NULL || heard[0] == '\0') return;
  char buf[160];
  snprintf(buf, sizeof(buf), "听到「%s」请重说", heard);
  lv_label_set_text(s_lbl_hint, buf);
}

