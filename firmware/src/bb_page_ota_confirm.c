/**
 * OTA confirm page. See bb_page_ota_confirm.h.
 *
 * Layout (320×172 display):
 *   Y= 18  "UPDATE?"  title (accent teal)
 *   Y= 44  "vA.B.C -> vX.Y.Z"  version line (cool white)
 *   Y= 62  "NNN KB"  size line (dim)
 *   Y= 86  dot countdown bar (30 cells, one dot extinguished per second)
 *   Y=108  "OK upgrade  BACK skip" hint (dim)
 *
 * Timer design: the LVGL 1 Hz timer ONLY updates the dot bar and sets
 * s_timed_out=1 when it reaches zero — it never calls user code.
 * bb_page_ota_confirm_handle_nav() (called from the radio-app main loop)
 * destroys the page and fires the callback outside the LVGL lock.
 */
#include "bb_page_ota_confirm.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_ui_layout.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#if defined(BBCLAW_SIMULATOR)
#define ESP_LOGI(tag, fmt, ...) ((void)(tag))
#define ESP_LOGW(tag, fmt, ...) ((void)(tag))
static int lvgl_port_lock(int t) { (void)t; return 1; }
static void lvgl_port_unlock(void) {}
#else
#include "esp_log.h"
#include "esp_lvgl_port.h"
#endif

static const char *TAG = "bb_page_ota_confirm";

/* ── palette ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_ACCENT    BB_UI_ACCENT
#define UI_DOT_LIT   BB_UI_DOT_LIT
#define UI_TEXT_DIM  BB_UI_TEXT_DIM

/* ── countdown bar geometry ── */
#define CDOWN_CELLS      30  /* 30 cells = 30 s，一秒灭一格（倒计时语义勿动） */
#if BB_UI_PORTRAIT
/* 竖屏手表（410×502）：dot/pitch 放大 ~1.6x，cell 数不变只放大几何 */
#define CDOWN_CELL_DOT    8
#define CDOWN_CELL_PITCH  12
#define CDOWN_CELL_RADIUS 2   /* 圆角随 cell 等比放大 */
#else
#define CDOWN_CELL_DOT    5
#define CDOWN_CELL_PITCH  8
#define CDOWN_CELL_RADIUS 1
#endif
#define CDOWN_BAR_W      ((CDOWN_CELLS - 1) * CDOWN_CELL_PITCH + CDOWN_CELL_DOT)

/* ── Y positions ── */
#if BB_UI_PORTRAIT
/* 内容组整体垂直居中（行距 ~1.5x）——居中构图天然避开 R60 物理圆角
 * （UI_DESIGN_LANGUAGE.md §2.1 / ADR-040 §UI）。 */
#define TITLE_Y    ((BB_DISP_H - 156) / 2)   /* 内容块高 ~156px，垂直居中 */
#define VER_Y      (TITLE_Y + 40)
#define SIZE_Y     (VER_Y + 28)
#define BAR_Y      (SIZE_Y + 36)
#define HINT_Y     (BAR_Y + CDOWN_CELL_DOT + 28)
#else
#define TITLE_Y    18
#define VER_Y      44
#define SIZE_Y     62
#define BAR_Y      86
#define HINT_Y    108
#endif

#define TIMEOUT_SEC 30

#if LV_FONT_MONTSERRAT_14
LV_FONT_DECLARE(lv_font_montserrat_14)
#endif

static const lv_font_t *small_font(void) {
#if LV_FONT_MONTSERRAT_14
  return &lv_font_montserrat_14;
#else
  return lv_font_get_default();
#endif
}

/* ── module state ── */
static lv_obj_t            *s_root;
static lv_obj_t            *s_cells[CDOWN_CELLS];
static lv_timer_t          *s_timer;
static volatile int         s_remaining;  /* seconds left; written by timer cb */
static volatile int         s_timed_out;  /* set to 1 by timer when done       */
static bb_page_ota_confirm_cb_t s_cb;

/* ── internal helpers (called with LVGL lock held) ── */

static void destroy_now(void) {
  if (s_timer) { lv_timer_del(s_timer); s_timer = NULL; }
  if (s_root)  { lv_obj_del(s_root);  s_root  = NULL; }
}

static void paint_countdown(int remaining) {
  for (int i = 0; i < CDOWN_CELLS; i++) {
    uint32_t col = (i < remaining) ? UI_ACCENT : UI_DOT_GHOST;
    lv_obj_set_style_bg_color(s_cells[i], lv_color_hex(col), 0);
  }
}

/* 1 Hz timer — only touches LVGL objects and volatile flags, never user cb. */
static void confirm_timer_cb(lv_timer_t *t) {
  (void)t;
  if (!s_root) return;
  int rem = s_remaining - 1;
  if (rem < 0) rem = 0;
  s_remaining = rem;
  paint_countdown(rem);
  if (rem == 0) s_timed_out = 1;
}

/* ── public API ── */

void bb_page_ota_confirm_show(const char *current_ver,
                              const char *new_ver,
                              uint32_t    size_bytes,
                              bb_page_ota_confirm_cb_t cb) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "show: lvgl lock timeout");
    return;
  }
  if (s_root) { lvgl_port_unlock(); return; }

  s_cb        = cb;
  s_remaining = TIMEOUT_SEC;
  s_timed_out = 0;

  /* Root */
  s_root = lv_obj_create(lv_layer_top());
  lv_obj_remove_style_all(s_root);
  lv_obj_set_size(s_root, BBCLAW_ST7789_WIDTH, BBCLAW_ST7789_HEIGHT);
  lv_obj_set_pos(s_root, 0, 0);
  lv_obj_set_style_bg_color(s_root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_root, LV_OBJ_FLAG_SCROLLABLE | LV_OBJ_FLAG_CLICKABLE);

  /* Title */
  lv_obj_t *title = lv_label_create(s_root);
  lv_obj_set_style_text_color(title, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_text_font(title, small_font(), 0);
  lv_obj_set_style_text_align(title, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(title, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(title, 0, TITLE_Y);
  lv_label_set_text(title, "UPDATE?");

  /* Version line */
  const char *cv = (current_ver && current_ver[0]) ? current_ver : "?";
  const char *nv = (new_ver     && new_ver[0])     ? new_ver     : "?";
  char ver_buf[80];
  snprintf(ver_buf, sizeof(ver_buf), "%s -> %s", cv, nv);
  lv_obj_t *ver = lv_label_create(s_root);
  lv_obj_set_style_text_color(ver, lv_color_hex(UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(ver, small_font(), 0);
  lv_obj_set_style_text_align(ver, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(ver, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(ver, 0, VER_Y);
  lv_label_set_text(ver, ver_buf);

  /* Size line */
  char size_buf[24];
  if (size_bytes >= 1024u * 1024u) {
    snprintf(size_buf, sizeof(size_buf), "%.1f MB",
             (double)size_bytes / (1024.0 * 1024.0));
  } else {
    snprintf(size_buf, sizeof(size_buf), "%u KB",
             (unsigned)((size_bytes + 512u) / 1024u));
  }
  lv_obj_t *sz = lv_label_create(s_root);
  lv_obj_set_style_text_color(sz, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(sz, small_font(), 0);
  lv_obj_set_style_text_align(sz, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(sz, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(sz, 0, SIZE_Y);
  lv_label_set_text(sz, size_buf);

  /* Countdown dot bar — all lit at start */
  int x0 = (BBCLAW_ST7789_WIDTH - CDOWN_BAR_W) / 2;
  for (int i = 0; i < CDOWN_CELLS; i++) {
    lv_obj_t *c = lv_obj_create(s_root);
    lv_obj_remove_style_all(c);
    lv_obj_set_size(c, CDOWN_CELL_DOT, CDOWN_CELL_DOT);
    lv_obj_set_pos(c, x0 + i * CDOWN_CELL_PITCH, BAR_Y);
    lv_obj_set_style_radius(c, CDOWN_CELL_RADIUS, 0);
    lv_obj_set_style_bg_color(c, lv_color_hex(UI_ACCENT), 0);
    lv_obj_set_style_bg_opa(c, LV_OPA_COVER, 0);
    lv_obj_clear_flag(c, LV_OBJ_FLAG_SCROLLABLE);
    s_cells[i] = c;
  }

  /* Hint */
  lv_obj_t *hint = lv_label_create(s_root);
  lv_obj_set_style_text_color(hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(hint, small_font(), 0);
  lv_obj_set_style_text_align(hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(hint, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(hint, 0, HINT_Y);
  lv_label_set_text(hint, "OK upgrade  BACK skip");

  s_timer = lv_timer_create(confirm_timer_cb, 1000, NULL);

  lvgl_port_unlock();
  ESP_LOGI(TAG, "confirm shown cur=%s new=%s size=%u", cv, nv, (unsigned)size_bytes);
}

void bb_page_ota_confirm_dismiss(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "dismiss: lvgl lock timeout");
    return;
  }
  s_cb = NULL;
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "confirm dismissed");
}

int bb_page_ota_confirm_active(void) {
  return s_root ? 1 : 0;
}

/** Returns 1 if the 30 s timeout fired and we're waiting for the main loop
 *  to call handle_nav(0) to dismiss + fire the callback. */
int bb_page_ota_confirm_timed_out(void) {
  return s_timed_out;
}

/** Route a nav decision.  nav_ok=1 → accept, nav_ok=0 → skip.
 *  Destroys the page, then calls the callback outside the LVGL lock.
 *  Safe to call when page is not active (no-op). */
void bb_page_ota_confirm_handle_nav(int nav_ok) {
  if (!s_root) return;
  if (!lvgl_port_lock(600)) {
    ESP_LOGW(TAG, "handle_nav: lvgl lock timeout");
    return;
  }
  int accept = nav_ok ? 1 : 0;
  bb_page_ota_confirm_cb_t cb = s_cb;
  s_cb = NULL;
  s_timed_out = 0;
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "confirm decision accept=%d", accept);
  if (cb) cb(accept);
}
