/**
 * STANDBY page — dot-matrix minimal (Nothing-style).
 *
 * The time is the hero: a 5×7 LED-matrix per digit, where the lit dots
 * forming each numeral emerge from a field of dim "ghost" dots. Monochrome
 * cool-white digits; the colon is the single teal accent and breathes
 * (opacity pulse). Footer carries a dim "bbclaw" wordmark + compact battery.
 *
 * Public API is unchanged (create / set_visible / refresh_clock /
 * update_battery) so bb_lvgl_display.c needs no edits.
 */
#include "bb_page_standby.h"

#include <ctype.h>
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_nav_input.h"
#include "bb_ota.h"
#include "bb_ui_layout.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#if LV_FONT_MONTSERRAT_14
LV_FONT_DECLARE(lv_font_montserrat_14)
#endif
#if LV_FONT_MONTSERRAT_40
LV_FONT_DECLARE(lv_font_montserrat_40)
#endif

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

/* ── palette — design/UI_DESIGN_LANGUAGE.md tokens ── */
#define UI_SCR_BG     BB_UI_BG        /* deep cool near-black            */
#define UI_DOT_LIT    BB_UI_DOT_LIT   /* cool white — active matrix dot  */
#define UI_DOT_GHOST  BB_UI_DOT_GHOST /* dim cool — inactive matrix dot  */
#define UI_ACCENT     BB_UI_ACCENT    /* teal — breathing colon          */
#define UI_WORDMARK   BB_UI_WORDMARK  /* dim teal-grey — footer wordmark */

/* ── dot-matrix geometry（竖屏手表 hero 放大 ~1.8x；方屏板保持原值） ── */
#if BB_UI_PORTRAIT
#if BB_DISP_W <= 160
/* 窄竖屏（M5StickS3 135px）：HH:MM = 4 位 + 冒号，dot 4 / pitch 5 → 整块
 * 118px 居中塞进 135（左右各 ~8px）。手表的 9/16 会宽到 371px 严重溢出。 */
#define MX_DOT     4
#define MX_PITCH   5
#define DIGIT_GAP  4
#define COLON_GAP  5
#else
#define MX_DOT     9
#define MX_PITCH   16
#define DIGIT_GAP  14          /* gap between paired digits        */
#define COLON_GAP  21          /* gap each side of the colon       */
#endif
#else
#define MX_DOT     5            /* dot diameter                    */
#define MX_PITCH   9            /* center-to-center spacing        */
#define DIGIT_GAP  8           /* gap between paired digits        */
#define COLON_GAP  12          /* gap each side of the colon       */
#endif
#define MX_COLS    5
#define MX_ROWS    7
#define DIGIT_W    ((MX_COLS - 1) * MX_PITCH + MX_DOT)  /* 41 / 73 */
#define DIGIT_H    ((MX_ROWS - 1) * MX_PITCH + MX_DOT)  /* 59 / 105 */
#define MATRIX_W   (4 * DIGIT_W + 2 * DIGIT_GAP + 2 * COLON_GAP + MX_DOT) /* 209 / 371 */
#define MATRIX_X   ((DISP_W - MATRIX_W) / 2)
#if BB_UI_PORTRAIT
/* 时钟 hero 居中偏上（~40% 高度，ADR-040 §UI.2） */
#define MATRIX_TOP (((DISP_H - DIGIT_H) * 2) / 5)
#else
#define MATRIX_TOP 26
#endif

/* ── compact footer battery ── */
#define FB_FRAME_W 26
#define FB_FRAME_H 12
#define FB_CAP_W    3
#define FB_CAP_H    6
#define FB_INSET    2
#if BB_UI_PORTRAIT
/* 底部居中簇：电池行 + 字标行 + 版本行（居中构图天然圆角安全） */
#define FB_Y (DISP_H - BB_UI_SAFE_BOTTOM - 56)
#else
#define FB_Y 148
#endif

/* 5×7 numerals, MSB→LSB = leftmost→rightmost of 5 columns. */
static const uint8_t GLYPH[10][MX_ROWS] = {
  {0x0E,0x11,0x13,0x15,0x19,0x11,0x0E}, /* 0 */
  {0x04,0x0C,0x04,0x04,0x04,0x04,0x0E}, /* 1 */
  {0x0E,0x11,0x01,0x02,0x04,0x08,0x1F}, /* 2 */
  {0x1F,0x02,0x04,0x02,0x01,0x11,0x0E}, /* 3 */
  {0x02,0x06,0x0A,0x12,0x1F,0x02,0x02}, /* 4 */
  {0x1F,0x10,0x1E,0x01,0x01,0x11,0x0E}, /* 5 */
  {0x06,0x08,0x10,0x1E,0x11,0x11,0x0E}, /* 6 */
  {0x1F,0x01,0x02,0x04,0x08,0x08,0x08}, /* 7 */
  {0x0E,0x11,0x11,0x0E,0x11,0x11,0x0E}, /* 8 */
  {0x0E,0x11,0x11,0x0F,0x01,0x02,0x0C}, /* 9 */
};

static lv_obj_t* s_view;
static lv_obj_t* s_dots[4][MX_ROWS][MX_COLS];
static lv_obj_t* s_colon[2];
static lv_obj_t* s_bat_box;
static lv_obj_t* s_bat_fill;
static lv_obj_t* s_bat_frame;
static lv_obj_t* s_bat_cap;
static lv_obj_t* s_bat_pct;
static lv_obj_t* s_notif_badge; /* unread-reminder badge, top-center (ADR-021 §9.3) */
static lv_obj_t* s_ambient_view;
static lv_obj_t* s_ambient_z;
static lv_obj_t* s_ambient_dots[3];
static lv_timer_t* s_ambient_timer;
static volatile int s_ambient_requested;
static int s_ambient_active;
static uint8_t s_ambient_phase;

/* 16-step ease-in/out pulse, deliberately quantized for a 5 FPS low-power
 * animation. The three dots use 120-degree-ish phase offsets. */
static const lv_opa_t AMBIENT_OPA[16] = {
    28, 34, 48, 72, 104, 144, 184, 216,
    232, 216, 184, 144, 104, 72, 48, 34,
};

static const lv_font_t* small_font_fn(void) {
#if LV_FONT_MONTSERRAT_14
  return &lv_font_montserrat_14;
#else
  return lv_font_get_default();
#endif
}

static const lv_font_t* ambient_z_font_fn(void) {
#if LV_FONT_MONTSERRAT_40
  return &lv_font_montserrat_40;
#else
  return small_font_fn();
#endif
}

/* Left edge x of digit slot 0..3 (HH:MM), colon sits between slots 1 and 2. */
static int digit_x(int slot) {
  int x = MATRIX_X + slot * (DIGIT_W + DIGIT_GAP);
  if (slot >= 2) x += 2 * COLON_GAP + MX_DOT - DIGIT_GAP;
  return x;
}

/* Build one digit's 5×7 dot grid as ghost dots (lit state set later). */
static void build_digit(int slot) {
  int x0 = digit_x(slot);
  for (int r = 0; r < MX_ROWS; r++) {
    for (int c = 0; c < MX_COLS; c++) {
      lv_obj_t* d = lv_obj_create(s_view);
      lv_obj_remove_style_all(d);
      lv_obj_set_size(d, MX_DOT, MX_DOT);
      lv_obj_set_pos(d, x0 + c * MX_PITCH, MATRIX_TOP + r * MX_PITCH);
      lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
      lv_obj_set_style_bg_color(d, lv_color_hex(UI_DOT_GHOST), 0);
      lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
      lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
      s_dots[slot][r][c] = d;
    }
  }
}

#define DIGIT_DASH (-2) /* centered dash — pre-SNTP "--:--" placeholder */

/* Light/dim the dots of one slot to render `digit` (0-9), a centered dash
 * (DIGIT_DASH), or all-ghost if any other negative value. */
static void set_digit(int slot, int digit) {
  for (int r = 0; r < MX_ROWS; r++) {
    uint8_t bits = 0;
    if (digit >= 0 && digit <= 9) bits = GLYPH[digit][r];
    else if (digit == DIGIT_DASH && r == MX_ROWS / 2) bits = 0x0E;
    for (int c = 0; c < MX_COLS; c++) {
      int on = (bits >> (MX_COLS - 1 - c)) & 1;
      lv_obj_set_style_bg_color(
          s_dots[slot][r][c],
          lv_color_hex(on ? UI_DOT_LIT : UI_DOT_GHOST), 0);
    }
  }
}

static void colon_pulse_cb(void* obj, int32_t v) {
  lv_obj_set_style_bg_opa((lv_obj_t*)obj, (lv_opa_t)v, 0);
}

/* Two teal dots centered in the colon gap, breathing in opacity. */
static void build_colon(void) {
  int cx = digit_x(1) + DIGIT_W + COLON_GAP;        /* gap center, x */
  int span = (MX_ROWS - 1) * MX_PITCH;              /* matrix height */
  int y_top = MATRIX_TOP + span / 2 - MX_PITCH;     /* rows 2 & 4-ish */
  int ys[2] = {y_top, y_top + 2 * MX_PITCH};
  for (int i = 0; i < 2; i++) {
    lv_obj_t* d = lv_obj_create(s_view);
    lv_obj_remove_style_all(d);
    lv_obj_set_size(d, MX_DOT, MX_DOT);
    lv_obj_set_pos(d, cx, ys[i]);
    lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(d, lv_color_hex(UI_ACCENT), 0);
    lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
    lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
    s_colon[i] = d;

    lv_anim_t a;
    lv_anim_init(&a);
    lv_anim_set_var(&a, d);
    lv_anim_set_values(&a, LV_OPA_30, LV_OPA_COVER);
    lv_anim_set_duration(&a, 1100);
    lv_anim_set_playback_duration(&a, 1100);
    lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
    lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
    lv_anim_set_exec_cb(&a, colon_pulse_cb);
    lv_anim_start(&a);
  }
}

static void standby_tap_cb(lv_event_t* e) {
  (void)e;
  bb_nav_input_inject(BB_NAV_EVENT_UP);
}

static void ambient_timer_cb(lv_timer_t* timer) {
  if (!s_ambient_requested) {
    if (s_ambient_view != NULL) lv_obj_add_flag(s_ambient_view, LV_OBJ_FLAG_HIDDEN);
    s_ambient_active = 0;
    lv_timer_pause(timer);
    return;
  }

  if (!s_ambient_active) {
    lv_obj_clear_flag(s_ambient_view, LV_OBJ_FLAG_HIDDEN);
    lv_obj_move_foreground(s_ambient_view);
    s_ambient_active = 1;
  }

  static const uint8_t offsets[3] = {0, 5, 10};
  lv_obj_set_style_text_opa(s_ambient_z, AMBIENT_OPA[s_ambient_phase], 0);
  for (int i = 0; i < 3; i++) {
    lv_obj_set_style_bg_opa(s_ambient_dots[i], AMBIENT_OPA[(s_ambient_phase + offsets[i]) & 0x0f], 0);
  }
  s_ambient_phase = (s_ambient_phase + 1) & 0x0f;
}

void bb_page_standby_set_ambient(int enabled) {
  s_ambient_requested = enabled != 0;
  /* Starting a paused LVGL timer from stream_task is unsafe. The normal clock
   * refresh below notices the request within one second and resumes it. While
   * active, the timer itself notices disable requests within 200 ms. */
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
  /* 触摸：待机点一下 = 唤醒进聊天（注入 UP,唤醒边沿被消费不透传;
   * 无触摸板上没有指针 indev,CLICKED 永不触发,零行为差异） */
  lv_obj_add_flag(s_view, LV_OBJ_FLAG_CLICKABLE);
  lv_obj_add_event_cb(s_view, standby_tap_cb, LV_EVENT_CLICKED, NULL);

  for (int slot = 0; slot < 4; slot++) build_digit(slot);
  build_colon();
  set_digit(0, -1);
  set_digit(1, -1);
  set_digit(2, -1);
  set_digit(3, -1);

  const lv_font_t* small_font = small_font_fn();

#if BB_UI_PORTRAIT
  /* 竖屏页脚：居中簇（电池行 FB_Y → 字标 → 版本，逐行向下） */
  lv_obj_t* mark = lv_label_create(s_view);
  lv_obj_set_style_text_color(mark, lv_color_hex(UI_WORDMARK), 0);
  lv_obj_set_style_text_font(mark, small_font, 0);
  lv_obj_set_width(mark, DISP_W);
  lv_obj_set_style_text_align(mark, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(mark, "bbclaw");
  lv_obj_set_pos(mark, 0, FB_Y + FB_FRAME_H + 10);

  lv_obj_t* ver = lv_label_create(s_view);
  lv_obj_set_style_text_color(ver, lv_color_hex(UI_WORDMARK), 0);
  lv_obj_set_style_text_font(ver, small_font, 0);
  lv_obj_set_width(ver, DISP_W);
  lv_obj_set_style_text_align(ver, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(ver, bb_ota_get_current_version());
  lv_obj_set_pos(ver, 0, FB_Y + FB_FRAME_H + 30);
#else
  /* Footer wordmark — lowercase, dim, left aligned */
  lv_obj_t* mark = lv_label_create(s_view);
  lv_obj_set_style_text_color(mark, lv_color_hex(UI_WORDMARK), 0);
  lv_obj_set_style_text_font(mark, small_font, 0);
  lv_label_set_text(mark, "bbclaw");
  lv_obj_set_pos(mark, 14, FB_Y - 3);

  /* Firmware version — small + dim, centered just above the footer row so it
   * never collides with the wordmark (left) or battery (right). Unobtrusive
   * build stamp so the running version is visible at a glance from the idle
   * screen. ASCII only (e.g. "v0.5.6"), so the small montserrat font is fine. */
  lv_obj_t* ver = lv_label_create(s_view);
  lv_obj_set_style_text_color(ver, lv_color_hex(UI_WORDMARK), 0);
  lv_obj_set_style_text_font(ver, small_font, 0);
  lv_obj_set_width(ver, DISP_W);
  lv_obj_set_style_text_align(ver, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(ver, bb_ota_get_current_version());
  lv_obj_set_pos(ver, 0, FB_Y - 22);
#endif

  /* __APPEND_CREATE__ */

  /* Unread-reminder badge — top-center, accent color, hidden when 0 (ADR-021
   * §9.3). The idle screen is where the user usually is when a reminder fires,
   * so this is the visible surface without opening the menu. ASCII label, small
   * font to match the wordmark/version styling. */
  s_notif_badge = lv_label_create(s_view);
  lv_obj_set_style_text_color(s_notif_badge, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_text_font(s_notif_badge, small_font, 0);
  lv_obj_set_width(s_notif_badge, DISP_W);
  lv_obj_set_style_text_align(s_notif_badge, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_notif_badge, "");
#if BB_UI_PORTRAIT
  /* 竖屏：徽标移到时钟下方居中（顶部中央贴边会碰圆角安全带边缘） */
  lv_obj_set_pos(s_notif_badge, 0, MATRIX_TOP + DIGIT_H + 26);
#else
  lv_obj_set_pos(s_notif_badge, 0, 6);
#endif
  lv_obj_add_flag(s_notif_badge, LV_OBJ_FLAG_HIDDEN);

#if BB_UI_PORTRAIT
  /* 竖屏：电池簇整体居中（百分比在左、图标在右） */
  const int bat_x = DISP_W / 2 + 4;
#else
  /* Compact footer battery — right edge; percent sits to its left. */
  const int bat_x = DISP_W - 14 - FB_FRAME_W - FB_CAP_W;
#endif

  s_bat_box = lv_obj_create(s_view);
  lv_obj_remove_style_all(s_bat_box);
  lv_obj_set_size(s_bat_box, FB_FRAME_W + FB_CAP_W, FB_FRAME_H);
  lv_obj_set_pos(s_bat_box, bat_x, FB_Y);
  lv_obj_clear_flag(s_bat_box, LV_OBJ_FLAG_SCROLLABLE);

  s_bat_fill = lv_obj_create(s_bat_box);
  lv_obj_remove_style_all(s_bat_fill);
  lv_obj_set_size(s_bat_fill, FB_FRAME_W - 2 * FB_INSET, FB_FRAME_H - 2 * FB_INSET);
  lv_obj_set_pos(s_bat_fill, FB_INSET, FB_INSET);
  lv_obj_set_style_radius(s_bat_fill, 1, 0);
  lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_bat_fill, LV_OPA_COVER, 0);

  s_bat_frame = lv_obj_create(s_bat_box);
  lv_obj_remove_style_all(s_bat_frame);
  lv_obj_set_size(s_bat_frame, FB_FRAME_W, FB_FRAME_H);
  lv_obj_set_style_radius(s_bat_frame, 2, 0);
  lv_obj_set_style_border_width(s_bat_frame, 2, 0);
  lv_obj_set_style_border_color(s_bat_frame, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_border_opa(s_bat_frame, LV_OPA_COVER, 0);
  lv_obj_set_style_bg_opa(s_bat_frame, LV_OPA_0, 0);
  lv_obj_clear_flag(s_bat_frame, LV_OBJ_FLAG_SCROLLABLE);

  s_bat_cap = lv_obj_create(s_bat_box);
  lv_obj_remove_style_all(s_bat_cap);
  lv_obj_set_size(s_bat_cap, FB_CAP_W, FB_CAP_H);
  lv_obj_set_pos(s_bat_cap, FB_FRAME_W, (FB_FRAME_H - FB_CAP_H) / 2);
  lv_obj_set_style_radius(s_bat_cap, 1, 0);
  lv_obj_set_style_bg_color(s_bat_cap, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_bat_cap, LV_OPA_COVER, 0);

  s_bat_pct = lv_label_create(s_view);
  lv_obj_set_style_text_color(s_bat_pct, lv_color_hex(UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(s_bat_pct, small_font, 0);
  lv_obj_set_style_text_align(s_bat_pct, LV_TEXT_ALIGN_RIGHT, 0);
  lv_obj_set_width(s_bat_pct, 42);
  lv_label_set_text(s_bat_pct, "");
  lv_obj_set_pos(s_bat_pct, bat_x - 42 - 8, FB_Y - 3);

  lv_obj_add_flag(s_bat_box, LV_OBJ_FLAG_HIDDEN);
  lv_obj_add_flag(s_bat_pct, LV_OBJ_FLAG_HIDDEN);

  /* Full-screen ambient overlay lives on the top layer so it also covers chat
   * or settings if the idle timeout expires there. It is normally hidden. */
  s_ambient_view = lv_obj_create(lv_layer_top());
  lv_obj_remove_style_all(s_ambient_view);
  lv_obj_set_size(s_ambient_view, DISP_W, DISP_H);
  lv_obj_set_pos(s_ambient_view, 0, 0);
  lv_obj_set_style_bg_color(s_ambient_view, lv_color_hex(BB_UI_BG), 0);
  lv_obj_set_style_bg_opa(s_ambient_view, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_ambient_view, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_add_flag(s_ambient_view, LV_OBJ_FLAG_CLICKABLE);
  lv_obj_add_event_cb(s_ambient_view, standby_tap_cb, LV_EVENT_CLICKED, NULL);

  const int dot_size = BB_UI_PORTRAIT ? 10 : 6;
  const int dot_gap = BB_UI_PORTRAIT ? 24 : 16;
  const int z_w = BB_UI_PORTRAIT ? 34 : 28;
  const int dots_w = dot_size * 3 + dot_gap * 2;
  const int group_w = z_w + dot_gap + dots_w;
  const int group_x = (DISP_W - group_w) / 2;
  const int group_y = DISP_H / 2;

  s_ambient_z = lv_label_create(s_ambient_view);
  lv_obj_set_style_text_color(s_ambient_z, lv_color_hex(UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(s_ambient_z, ambient_z_font_fn(), 0);
  lv_obj_set_style_text_opa(s_ambient_z, AMBIENT_OPA[0], 0);
  lv_label_set_text(s_ambient_z, "Z");
  lv_obj_set_pos(s_ambient_z, group_x, group_y - 22);

  for (int i = 0; i < 3; i++) {
    lv_obj_t* dot = lv_obj_create(s_ambient_view);
    lv_obj_remove_style_all(dot);
    lv_obj_set_size(dot, dot_size, dot_size);
    lv_obj_set_pos(dot, group_x + z_w + dot_gap + i * (dot_size + dot_gap),
                   group_y - dot_size / 2);
    lv_obj_set_style_radius(dot, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(dot, lv_color_hex(UI_DOT_LIT), 0);
    lv_obj_set_style_bg_opa(dot, AMBIENT_OPA[i * 5], 0);
    s_ambient_dots[i] = dot;
  }
  lv_obj_add_flag(s_ambient_view, LV_OBJ_FLAG_HIDDEN);
  s_ambient_timer = lv_timer_create(ambient_timer_cb, 200, NULL);
  lv_timer_pause(s_ambient_timer);
}

/* __APPEND__ */

void bb_page_standby_set_unread(int count) {
  if (s_notif_badge == NULL) return;
  /* Idempotent: this is polled every second on the standby clock tick, so skip
   * the relabel/relayout when the count hasn't changed. */
  static int last = -1;
  if (count == last) return;
  last = count;
  if (count > 0) {
    char buf[24];
    snprintf(buf, sizeof(buf), count == 1 ? "%d reminder" : "%d reminders", count);
    lv_label_set_text(s_notif_badge, buf);
    lv_obj_clear_flag(s_notif_badge, LV_OBJ_FLAG_HIDDEN);
  } else {
    lv_obj_add_flag(s_notif_badge, LV_OBJ_FLAG_HIDDEN);
  }
}

void bb_page_standby_set_visible(int visible) {
  if (s_view == NULL) return;
  if (visible) lv_obj_clear_flag(s_view, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(s_view, LV_OBJ_FLAG_HIDDEN);
}

void bb_page_standby_refresh_clock(const char* hm) {
  if (s_dots[0][0][0] == NULL || hm == NULL) return;

  /* This function runs in the LVGL task once per second. It is the safe bridge
   * that starts the paused ambient animation requested by stream_task. */
  if (s_ambient_requested && s_ambient_timer != NULL && !s_ambient_active) {
    lv_timer_resume(s_ambient_timer);
    lv_timer_ready(s_ambient_timer);
  }

  /* Pull up to 4 ASCII digits out of "HH:MM" (tolerates "H:MM"/junk). */
  int d[4] = {-1, -1, -1, -1};
  int n = 0;
  for (const char* p = hm; *p && n < 4; ++p) {
    if (isdigit((unsigned char)*p)) d[n++] = *p - '0';
  }
  /* Right-align so "9:05" → _9:05 keeps minutes in the last two slots. */
  if (n == 3) { d[3] = d[2]; d[2] = d[1]; d[1] = d[0]; d[0] = -1; }
  /* No digits at all ("--:--", wall time not ready) → centered dashes,
   * not an all-ghost (≈black) screen. */
  if (n == 0) { d[0] = d[1] = d[2] = d[3] = DIGIT_DASH; }

  for (int slot = 0; slot < 4; slot++) set_digit(slot, d[slot]);
}

void bb_page_standby_update_battery(int supported, int available, int percent, int low, int charging) {
  if (s_bat_box == NULL || s_bat_fill == NULL) return;

  if (!supported || !available || percent < 0) {
    lv_obj_add_flag(s_bat_box, LV_OBJ_FLAG_HIDDEN);
    if (s_bat_pct) lv_obj_add_flag(s_bat_pct, LV_OBJ_FLAG_HIDDEN);
    return;
  }

  lv_obj_clear_flag(s_bat_box, LV_OBJ_FLAG_HIDDEN);
  if (s_bat_pct) lv_obj_clear_flag(s_bat_pct, LV_OBJ_FLAG_HIDDEN);

  if (percent > 100) percent = 100;
  if (percent < 0) percent = 0;

  uint32_t color = charging ? 0x4cd964 : (low ? 0xe66f6f : UI_ACCENT);
  int track_w = FB_FRAME_W - 2 * FB_INSET;
  int fill_w = (percent * track_w) / 100;
  if (fill_w < 1 && percent > 0) fill_w = 1;

  /* 充电动效：填充条从「当前电量」到满格循环生长（绿色呼吸式注入），
   * 直观表达「正在充、往哪充」。停充即删动画、回静态电量宽度。
   * update 周期性重入(电量轮询)：仅在充电状态翻转时启停,避免动画反复重建。 */
  static int s_chg_anim_on = 0;
  if (charging && !s_chg_anim_on) {
    lv_anim_t a;
    lv_anim_init(&a);
    lv_anim_set_var(&a, s_bat_fill);
    lv_anim_set_values(&a, fill_w, track_w);
    lv_anim_set_duration(&a, 1600);
    lv_anim_set_repeat_delay(&a, 400); /* 满格停一拍再从电量位重来 */
    lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
    lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
    lv_anim_set_exec_cb(&a, (lv_anim_exec_xcb_t)lv_obj_set_width);
    lv_anim_start(&a);
    s_chg_anim_on = 1;
  } else if (!charging && s_chg_anim_on) {
    lv_anim_delete(s_bat_fill, (lv_anim_exec_xcb_t)lv_obj_set_width);
    s_chg_anim_on = 0;
  }
  if (!charging) lv_obj_set_width(s_bat_fill, fill_w);
  lv_obj_set_style_bg_color(s_bat_fill, lv_color_hex(color), 0);
  lv_obj_set_style_border_color(s_bat_frame, lv_color_hex(color), 0);
  lv_obj_set_style_bg_color(s_bat_cap, lv_color_hex(color), 0);

  if (s_bat_pct) {
    char buf[16];
    snprintf(buf, sizeof(buf), "%d%%", percent);
    lv_label_set_text(s_bat_pct, buf);
    lv_obj_set_style_text_color(s_bat_pct, lv_color_hex(color), 0);
  }
}
