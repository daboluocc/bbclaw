/**
 * OTA page — dot-matrix firmware-update progress. See bb_page_ota.h.
 *
 * Visual language matches bb_page_boot.c / bb_page_netconn.c: a centered row
 * of dot cells fills left→right with the download, the leading filled cell
 * flashing teal (the "newest element flashes, settles cool white next beat"
 * motif), under an "UPDATING" title, with a "NN%" readout and the target
 * version. The page lives on lv_layer_top() and ignores input.
 *
 * Lives on the LVGL task via an internal timer that weakly reads the
 * download task's published percent — no cross-task lock on the hot path.
 */
#include "bb_page_ota.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#if defined(BBCLAW_SIMULATOR)
#define ESP_LOGI(tag, fmt, ...) ((void)(tag))
#define ESP_LOGW(tag, fmt, ...) ((void)(tag))
static int lvgl_port_lock(int timeout_ms) {
  (void)timeout_ms;
  return 1;
}
static void lvgl_port_unlock(void) {}
#else
#include "esp_log.h"
#include "esp_lvgl_port.h"
#endif

#if LV_FONT_MONTSERRAT_14
LV_FONT_DECLARE(lv_font_montserrat_14)
#endif

static const char* TAG = "bb_page_ota";

/* ── palette — design/UI_DESIGN_LANGUAGE.md tokens ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_DOT_LIT   BB_UI_DOT_LIT
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_ACCENT    BB_UI_ACCENT
#define UI_TEXT_DIM  BB_UI_TEXT_DIM

/* ── progress-bar geometry — a row of dot cells ── */
#define OTA_CELLS      24
#define OTA_CELL_DOT   6
#define OTA_CELL_PITCH 9
#define OTA_BAR_W      ((OTA_CELLS - 1) * OTA_CELL_PITCH + OTA_CELL_DOT)
#define OTA_BAR_Y      104
#define OTA_TITLE_Y    62
#define OTA_PCT_Y      120
#define OTA_VER_Y      146

#define OTA_TICK_MS 120

enum { PHASE_DOWNLOAD = 0, PHASE_DONE };

static lv_obj_t* s_root;
static lv_obj_t* s_cells[OTA_CELLS];
static lv_obj_t* s_title;
static lv_obj_t* s_pct;
static lv_obj_t* s_ver;
static lv_timer_t* s_timer;

/* Published by the download task; weakly read by the timer (single int). */
static volatile int s_want_percent;
static volatile int s_phase;
static int s_drawn_percent;
static int s_drawn_phase;

static const lv_font_t* small_font_fn(void) {
#if LV_FONT_MONTSERRAT_14
  return &lv_font_montserrat_14;
#else
  return lv_font_get_default();
#endif
}

static void destroy_now(void) {
  if (s_timer != NULL) {
    lv_timer_del(s_timer);
    s_timer = NULL;
  }
  if (s_root != NULL) {
    lv_obj_del(s_root);
    s_root = NULL;
  }
  s_title = NULL;
  s_pct = NULL;
  s_ver = NULL;
}

static void paint_cells(int pct) {
  int lit = (pct * OTA_CELLS) / 100;
  for (int i = 0; i < OTA_CELLS; i++) {
    uint32_t color;
    if (i < lit - 1) {
      color = UI_DOT_LIT;       /* settled */
    } else if (i == lit - 1) {
      color = UI_ACCENT;        /* leading edge flashes teal */
    } else {
      color = UI_DOT_GHOST;     /* not yet reached */
    }
    lv_obj_set_style_bg_color(s_cells[i], lv_color_hex(color), 0);
  }
}

static void ota_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_root == NULL) return;

  int phase = s_phase;
  int pct = s_want_percent;
  if (pct < 0) pct = 0;
  if (pct > 100) pct = 100;

  if (phase == s_drawn_phase && pct == s_drawn_percent) return;
  s_drawn_phase = phase;
  s_drawn_percent = pct;

  if (phase == PHASE_DONE) {
    for (int i = 0; i < OTA_CELLS; i++) {
      lv_obj_set_style_bg_color(s_cells[i], lv_color_hex(UI_ACCENT), 0);
    }
    lv_label_set_text(s_title, "DONE");
    lv_label_set_text(s_pct, "REBOOTING");
    return;
  }

  paint_cells(pct);
  char buf[8];
  snprintf(buf, sizeof(buf), "%d%%", pct);
  lv_label_set_text(s_pct, buf);
}

void bb_page_ota_show(const char* version) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "show: lvgl lock timeout — skipping ota page");
    return;
  }
  if (s_root != NULL) {
    lvgl_port_unlock();
    return;
  }

  s_want_percent = 0;
  s_phase = PHASE_DOWNLOAD;
  s_drawn_percent = -1;
  s_drawn_phase = -1;

  s_root = lv_obj_create(lv_layer_top());
  lv_obj_remove_style_all(s_root);
  lv_obj_set_size(s_root, BBCLAW_ST7789_WIDTH, BBCLAW_ST7789_HEIGHT);
  lv_obj_set_pos(s_root, 0, 0);
  lv_obj_set_style_bg_color(s_root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_root, LV_OBJ_FLAG_SCROLLABLE | LV_OBJ_FLAG_CLICKABLE);

  s_title = lv_label_create(s_root);
  lv_obj_set_style_text_color(s_title, lv_color_hex(UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(s_title, small_font_fn(), 0);
  lv_obj_set_style_text_align(s_title, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_title, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(s_title, 0, OTA_TITLE_Y);
  lv_label_set_text(s_title, "UPDATING");

  /* Ghost progress cells — the timer recolors them. */
  int x0 = (BBCLAW_ST7789_WIDTH - OTA_BAR_W) / 2;
  for (int i = 0; i < OTA_CELLS; i++) {
    lv_obj_t* c = lv_obj_create(s_root);
    lv_obj_remove_style_all(c);
    lv_obj_set_size(c, OTA_CELL_DOT, OTA_CELL_DOT);
    lv_obj_set_pos(c, x0 + i * OTA_CELL_PITCH, OTA_BAR_Y);
    lv_obj_set_style_radius(c, 1, 0);
    lv_obj_set_style_bg_color(c, lv_color_hex(UI_DOT_GHOST), 0);
    lv_obj_set_style_bg_opa(c, LV_OPA_COVER, 0);
    lv_obj_clear_flag(c, LV_OBJ_FLAG_SCROLLABLE);
    s_cells[i] = c;
  }

  s_pct = lv_label_create(s_root);
  lv_obj_set_style_text_color(s_pct, lv_color_hex(UI_ACCENT), 0);
  lv_obj_set_style_text_font(s_pct, small_font_fn(), 0);
  lv_obj_set_style_text_align(s_pct, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_pct, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(s_pct, 0, OTA_PCT_Y);
  lv_label_set_text(s_pct, "0%");

  s_ver = lv_label_create(s_root);
  lv_obj_set_style_text_color(s_ver, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_ver, small_font_fn(), 0);
  lv_obj_set_style_text_align(s_ver, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_ver, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(s_ver, 0, OTA_VER_Y);
  lv_label_set_text(s_ver, (version != NULL && version[0] != '\0') ? version : "");

  s_timer = lv_timer_create(ota_timer_cb, OTA_TICK_MS, NULL);

  lvgl_port_unlock();
  ESP_LOGI(TAG, "ota page shown version=%s", (version != NULL) ? version : "");
}

void bb_page_ota_set_progress(int percent) {
  if (percent < 0) percent = 0;
  if (percent > 100) percent = 100;
  s_want_percent = percent;
}

void bb_page_ota_set_done(void) {
  s_phase = PHASE_DONE;
}

void bb_page_ota_dismiss(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "dismiss: lvgl lock timeout — page stays");
    return;
  }
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "ota page dismissed");
}

int bb_page_ota_active(void) {
  return s_root != NULL ? 1 : 0;
}
