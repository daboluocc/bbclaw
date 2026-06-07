/**
 * BOOT splash — dot-matrix "BBCLAW" wordmark, Nokia-style reveal.
 *
 * Visual language matches bb_page_standby.c (dot 5px / pitch 9px / 5×7
 * glyphs): the full ghost grid appears at once, then lit dots sweep in
 * column-by-column left→right — the newest column flashes teal and settles
 * to cool white one tick later — and a teal underline grows under the
 * wordmark as the finishing beat.
 *
 * The splash lives on lv_layer_top() so it covers whichever base view
 * (LOCKED / STANDBY / ACTIVE) the app boots into. bb_page_boot_dismiss()
 * destroys it synchronously (hard cut, no fade — see the comment inside).
 * Voice sync (boot wav delayed until the sweep finishes) is the caller's
 * job — see bb_radio_app_start and design/STATE_MACHINE.md §3.5.
 */
#include "bb_page_boot.h"

#include "bb_config.h"
#include "bb_ui_theme.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "lvgl.h"

static const char* TAG = "bb_page_boot";

/* ── palette — design/UI_DESIGN_LANGUAGE.md tokens ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_DOT_LIT   BB_UI_DOT_LIT
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_ACCENT    BB_UI_ACCENT

/* ── dot-matrix geometry ── */
#define MX_DOT       5
#define MX_PITCH     9
#define MX_COLS      5
#define MX_ROWS      7
#define LETTER_COUNT 6                                   /* B B C L A W */
#define LETTER_W     ((MX_COLS - 1) * MX_PITCH + MX_DOT) /* 41 */
#define LETTER_H     ((MX_ROWS - 1) * MX_PITCH + MX_DOT) /* 59 */
#define LETTER_GAP   8
#define WORDMARK_W   (LETTER_COUNT * LETTER_W + (LETTER_COUNT - 1) * LETTER_GAP) /* 286 */
#define WORDMARK_X   ((BBCLAW_ST7789_WIDTH - WORDMARK_W) / 2)
#define WORDMARK_Y   ((BBCLAW_ST7789_HEIGHT - LETTER_H) / 2 - 8)
#define UNDERLINE_Y  (WORDMARK_Y + LETTER_H + 14)
#define UNDERLINE_H  3

/* ── timing ── */
#define BOOT_TICK_MS      35                       /* one column per tick   */
#define TOTAL_COLS        (LETTER_COUNT * MX_COLS) /* 30 → sweep ≈ 1.05 s   */
#define UNDERLINE_STEP_PX 26                       /* grow ≈ 0.4 s          */

/* 5×7 letter glyphs, MSB→LSB = leftmost→rightmost of 5 columns. */
static const uint8_t GLYPH_B[MX_ROWS] = {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E};
static const uint8_t GLYPH_C[MX_ROWS] = {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E};
static const uint8_t GLYPH_L[MX_ROWS] = {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F};
static const uint8_t GLYPH_A[MX_ROWS] = {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11};
static const uint8_t GLYPH_W[MX_ROWS] = {0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0A};

static const uint8_t* const WORDMARK[LETTER_COUNT] = {
    GLYPH_B, GLYPH_B, GLYPH_C, GLYPH_L, GLYPH_A, GLYPH_W,
};

static lv_obj_t* s_root;
static lv_obj_t* s_dots[LETTER_COUNT][MX_ROWS][MX_COLS];
static lv_obj_t* s_underline;
static lv_timer_t* s_timer;
static int s_reveal_col;    /* next global column (0..TOTAL_COLS) to light  */
static int s_underline_w;

/* Paint the glyph dots of one global column (letter*5+col) in `color`.
 * Non-glyph dots stay ghost. */
static void paint_col(int global_col, uint32_t color) {
  int letter = global_col / MX_COLS;
  int c = global_col % MX_COLS;
  if (letter < 0 || letter >= LETTER_COUNT) return;
  const uint8_t* glyph = WORDMARK[letter];
  for (int r = 0; r < MX_ROWS; r++) {
    if ((glyph[r] >> (MX_COLS - 1 - c)) & 1) {
      lv_obj_set_style_bg_color(s_dots[letter][r][c], lv_color_hex(color), 0);
    }
  }
}

static void boot_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_root == NULL) return;

  if (s_reveal_col < TOTAL_COLS) {
    /* Settle the previous column from teal to cool white, flash this one. */
    if (s_reveal_col > 0) paint_col(s_reveal_col - 1, UI_DOT_LIT);
    paint_col(s_reveal_col, UI_ACCENT);
    s_reveal_col++;
    return;
  }
  if (s_reveal_col == TOTAL_COLS) {
    /* One extra tick so the final column also gets its teal beat. */
    paint_col(TOTAL_COLS - 1, UI_DOT_LIT);
    s_reveal_col++;
    return;
  }

  /* Finishing beat: grow the teal underline. */
  if (s_underline_w < WORDMARK_W) {
    s_underline_w += UNDERLINE_STEP_PX;
    if (s_underline_w > WORDMARK_W) s_underline_w = WORDMARK_W;
    lv_obj_set_width(s_underline, s_underline_w);
    return;
  }

  /* Animation complete — idle until dismiss. */
  lv_timer_del(s_timer);
  s_timer = NULL;
}

static void destroy_locked(void) {
  if (s_timer != NULL) {
    lv_timer_del(s_timer);
    s_timer = NULL;
  }
  if (s_root != NULL) {
    lv_obj_del(s_root);
    s_root = NULL;
  }
  s_underline = NULL;
}

void bb_page_boot_show(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "show: lvgl lock timeout — skipping splash");
    return;
  }
  if (s_root != NULL) {
    lvgl_port_unlock();
    return;
  }

  s_root = lv_obj_create(lv_layer_top());
  lv_obj_remove_style_all(s_root);
  lv_obj_set_size(s_root, BBCLAW_ST7789_WIDTH, BBCLAW_ST7789_HEIGHT);
  lv_obj_set_pos(s_root, 0, 0);
  lv_obj_set_style_bg_color(s_root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_root, LV_OBJ_FLAG_SCROLLABLE | LV_OBJ_FLAG_CLICKABLE);

  /* Ghost grid — all six letters' 5×7 dots, dim. Sweep recolors them. */
  for (int l = 0; l < LETTER_COUNT; l++) {
    int x0 = WORDMARK_X + l * (LETTER_W + LETTER_GAP);
    for (int r = 0; r < MX_ROWS; r++) {
      for (int c = 0; c < MX_COLS; c++) {
        lv_obj_t* d = lv_obj_create(s_root);
        lv_obj_remove_style_all(d);
        lv_obj_set_size(d, MX_DOT, MX_DOT);
        lv_obj_set_pos(d, x0 + c * MX_PITCH, WORDMARK_Y + r * MX_PITCH);
        lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
        lv_obj_set_style_bg_color(d, lv_color_hex(UI_DOT_GHOST), 0);
        lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
        lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
        s_dots[l][r][c] = d;
      }
    }
  }

  /* Teal underline — grows after the sweep completes. */
  s_underline = lv_obj_create(s_root);
  lv_obj_remove_style_all(s_underline);
  lv_obj_set_size(s_underline, 1, UNDERLINE_H);
  lv_obj_set_pos(s_underline, WORDMARK_X, UNDERLINE_Y);
  lv_obj_set_style_radius(s_underline, 2, 0);
  lv_obj_set_style_bg_color(s_underline, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_underline, LV_OPA_COVER, 0);

  s_reveal_col = 0;
  s_underline_w = 1;
  s_timer = lv_timer_create(boot_timer_cb, BOOT_TICK_MS, NULL);

  lvgl_port_unlock();
  ESP_LOGI(TAG, "splash shown (sweep %d cols × %d ms)", TOTAL_COLS, BOOT_TICK_MS);
}

void bb_page_boot_dismiss(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "dismiss: lvgl lock timeout — splash stays until next try");
    return;
  }
  /* Synchronous, no fade — deliberately. A parent-opa fade makes LVGL
   * composite the full screen through a transient layer buffer, and the
   * fade window used to overlap esp_wifi_init, whose 10×1600 B static RX
   * DMA buffers then failed with ESP_ERR_NO_MEM → boot loop. The splash
   * and the standby/locked views share a near-black background, so a hard
   * cut is visually fine. After this returns, every splash resource has
   * been freed. */
  destroy_locked();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "splash dismissed");
}

int bb_page_boot_active(void) {
  return s_root != NULL ? 1 : 0;
}

int bb_page_boot_anim_done(void) {
  /* boot_timer_cb deletes s_timer on its final tick, so timer-gone means the
   * sweep + underline have fully rendered. Benign race: both fields are only
   * written under the LVGL lock, and a stale read here just delays the
   * caller's poll by one step. */
  if (s_root == NULL) return 1;
  return s_timer == NULL ? 1 : 0;
}
