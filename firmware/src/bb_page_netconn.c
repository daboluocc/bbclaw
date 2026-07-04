/**
 * NETCONN page — dot-matrix WiFi arcs + the SSID currently being tried.
 *
 * Visual language matches bb_page_boot.c / bb_page_standby.c (5px dots,
 * same palette): a WiFi glyph — base dot + three concentric dot-arcs —
 * lights up ring-by-ring bottom→top while connecting (newest ring flashes
 * teal, settles cool white next beat, then the glyph resets to ghost and
 * loops). A label under the glyph shows `WiFi  <ssid>`, polled from
 * bb_wifi_get_active_ssid() so it follows slot retries live.
 *
 * Phases: connecting (loop) → connected (all rings hold teal, label
 * "SYNC TIME") → self-dismiss when bb_wall_time_ready(), or
 * BBCLAW_NETCONN_SYNC_TIMEOUT_MS after connect as a fallback — the standby
 * clock renders dashes for "--:--" so even the fallback isn't a black
 * screen. Provisioning / wifi-failure paths dismiss explicitly from
 * bb_radio_app_start. See design/STATE_MACHINE.md §3.5.1.
 *
 * The page lives on lv_layer_top(). Show/dismiss are synchronous hard
 * cuts — no opa fade, which would composite through a transient
 * full-screen layer buffer and collide with esp_wifi_init's internal DMA
 * allocations (the bb_page_boot NO_MEM boot-loop lesson).
 */
#include "bb_page_netconn.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_time.h"
#include "bb_ui_layout.h"
#include "bb_ui_theme.h"
#include "bb_wifi.h"
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

static const char* TAG = "bb_page_netconn";

/* ── palette — design/UI_DESIGN_LANGUAGE.md tokens ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_DOT_LIT   BB_UI_DOT_LIT
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_ACCENT    BB_UI_ACCENT

/* ── WiFi glyph geometry — base dot + 3 concentric dot-arcs ── */
#if BB_UI_PORTRAIT
/* 竖屏手表：glyph 放大 2x（WiFi 弧本身紧凑，取放大区间上限），glyph 上 + 状态文字下
 * 整组垂直居中（居中构图天然避开 R60 物理圆角，ADR-040 §UI）。 */
#define MX_DOT        10
#define RING_SCALE(v) ((v) * 2)
#define GLYPH_BASE_Y  ((BB_DISP_H * 51) / 100) /* base-dot center — 组合光学居中 */
#define LABEL_Y       (GLYPH_BASE_Y + 40)
#else
#define MX_DOT        5
#define RING_SCALE(v) (v)
#define GLYPH_BASE_Y  92 /* base-dot center, y */
#define LABEL_Y       (GLYPH_BASE_Y + 20)
#endif
#define RING_COUNT   4 /* ring 0 = base dot */
#define RING_MAX_DOT 7
#define GLYPH_CX     (BBCLAW_ST7789_WIDTH / 2)

/* Dot-center offsets from the base dot, per ring (x right, y up-negative).
 * Radii 12/22/32 px, dots spread along a ~100° upward arc. */
typedef struct {
  int8_t dx;
  int8_t dy;
} ring_off_t;

static const ring_off_t RING1[] = {{-8, -9}, {0, -12}, {8, -9}};
static const ring_off_t RING2[] = {{-16, -16}, {-8, -20}, {0, -22}, {8, -20}, {16, -16}};
static const ring_off_t RING3[] = {{-25, -21}, {-18, -27}, {-9, -31}, {0, -32}, {9, -31}, {18, -27}, {25, -21}};
static const ring_off_t RING0[] = {{0, 0}};

static const ring_off_t* const RINGS[RING_COUNT] = {RING0, RING1, RING2, RING3};
static const int RING_DOTS[RING_COUNT] = {1, 3, 5, 7};

/* ── timing ── */
#define NETCONN_TICK_MS 420 /* one ring per tick while connecting */

/* Connecting-loop steps: rings 0..3 light up, one hold beat, then reset. */
#define STEP_HOLD  RING_COUNT
#define STEP_RESET (RING_COUNT + 1)

static lv_obj_t* s_root;
static lv_obj_t* s_dots[RING_COUNT][RING_MAX_DOT];
static lv_obj_t* s_label;
static lv_timer_t* s_timer;
static int s_step;
static int s_connected_phase;
static int64_t s_connected_at_ms;
static char s_label_text[64];

static void paint_ring(int ring, uint32_t color) {
  if (ring < 0 || ring >= RING_COUNT) return;
  for (int i = 0; i < RING_DOTS[ring]; i++) {
    lv_obj_set_style_bg_color(s_dots[ring][i], lv_color_hex(color), 0);
  }
}

static const lv_font_t* small_font_fn(void) {
#if LV_FONT_MONTSERRAT_14
  return &lv_font_montserrat_14;
#else
  return lv_font_get_default();
#endif
}

/* Runs in the LVGL task — no port lock needed (and none taken: the port
 * mutex is held by the task around lv_timer_handler already). */
static void destroy_now(void) {
  if (s_timer != NULL) {
    lv_timer_del(s_timer);
    s_timer = NULL;
  }
  if (s_root != NULL) {
    lv_obj_del(s_root);
    s_root = NULL;
  }
  s_label = NULL;
}

static void refresh_label(void) {
  char next[sizeof(s_label_text)];
  if (s_connected_phase) {
    snprintf(next, sizeof(next), "SYNC TIME");
  } else {
    /* Weakly-consistent read of the app task's s_active_ssid — same class
     * of sharing as the AP-info display; a torn read shows for one tick. */
    const char* ssid = bb_wifi_get_active_ssid();
    if (ssid != NULL && ssid[0] != '\0') {
      snprintf(next, sizeof(next), "WiFi  %s", ssid);
    } else {
      snprintf(next, sizeof(next), "WiFi ...");
    }
  }
  if (strcmp(next, s_label_text) != 0) {
    snprintf(s_label_text, sizeof(s_label_text), "%s", next);
    lv_label_set_text(s_label, s_label_text);
  }
}

static void netconn_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_root == NULL) return;

  if (!s_connected_phase && bb_wifi_is_connected()) {
    /* Connected — hold the full glyph in teal and switch to time-sync. */
    s_connected_phase = 1;
    s_connected_at_ms = bb_now_ms();
    for (int r = 0; r < RING_COUNT; r++) paint_ring(r, UI_ACCENT);
    ESP_LOGI(TAG, "wifi connected — waiting for wall time");
  }

  refresh_label();

  if (s_connected_phase) {
    int64_t waited = bb_now_ms() - s_connected_at_ms;
    if (bb_wall_time_ready() || waited >= BBCLAW_NETCONN_SYNC_TIMEOUT_MS) {
      ESP_LOGI(TAG, "self-dismiss (time %s after %lld ms)", bb_wall_time_ready() ? "ready" : "timeout",
               (long long)waited);
      destroy_now();
    }
    return;
  }

  /* Connecting loop: light rings bottom→top, settle the previous ring. */
  if (s_step < RING_COUNT) {
    if (s_step > 0) paint_ring(s_step - 1, UI_DOT_LIT);
    paint_ring(s_step, UI_ACCENT);
  } else if (s_step == STEP_HOLD) {
    paint_ring(RING_COUNT - 1, UI_DOT_LIT);
  } else {
    for (int r = 0; r < RING_COUNT; r++) paint_ring(r, UI_DOT_GHOST);
  }
  s_step = (s_step >= STEP_RESET) ? 0 : s_step + 1;
}

void bb_page_netconn_show(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "show: lvgl lock timeout — skipping netconn page");
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

  /* Ghost glyph — base dot + 3 dot-arcs. The timer recolors them. */
  for (int r = 0; r < RING_COUNT; r++) {
    for (int i = 0; i < RING_DOTS[r]; i++) {
      lv_obj_t* d = lv_obj_create(s_root);
      lv_obj_remove_style_all(d);
      lv_obj_set_size(d, MX_DOT, MX_DOT);
      lv_obj_set_pos(d, GLYPH_CX + RING_SCALE(RINGS[r][i].dx) - MX_DOT / 2,
                     GLYPH_BASE_Y + RING_SCALE(RINGS[r][i].dy) - MX_DOT / 2);
      lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
      lv_obj_set_style_bg_color(d, lv_color_hex(UI_DOT_GHOST), 0);
      lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
      lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
      s_dots[r][i] = d;
    }
  }

  s_label = lv_label_create(s_root);
  lv_obj_set_style_text_color(s_label, lv_color_hex(UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(s_label, small_font_fn(), 0);
  lv_obj_set_style_text_align(s_label, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_label, BBCLAW_ST7789_WIDTH - 40);
  lv_obj_set_pos(s_label, 20, LABEL_Y);
  s_label_text[0] = '\0';
  lv_label_set_text(s_label, "WiFi ...");

  /* Light the base dot immediately so the very first frame already reads
   * as "connecting" instead of an all-ghost beat. */
  paint_ring(0, UI_ACCENT);
  s_step = 1;
  s_connected_phase = 0;
  s_connected_at_ms = 0;
  s_timer = lv_timer_create(netconn_timer_cb, NETCONN_TICK_MS, NULL);

  lvgl_port_unlock();
  ESP_LOGI(TAG, "netconn page shown");
}

void bb_page_netconn_dismiss(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "dismiss: lvgl lock timeout — page stays until next try");
    return;
  }
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "netconn page dismissed");
}

int bb_page_netconn_active(void) {
  return s_root != NULL ? 1 : 0;
}
