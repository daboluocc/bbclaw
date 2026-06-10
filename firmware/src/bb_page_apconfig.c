/**
 * APCONFIG page — dot-matrix WiFi 配网 screen. See bb_page_apconfig.h.
 *
 * Layout (320×172): a broadcasting WiFi glyph on the left (base dot + three
 * concentric dot-arcs that ripple outward bottom→top, recycled from
 * bb_page_netconn so the visual vocabulary matches) and a three-step join
 * guide on the right:
 *
 *      ((•))      WiFi 配网
 *       arcs      1  热点   BBClaw-C7EB88
 *                 2  密码   12345678
 *                 3  打开   192.168.4.1
 *
 * The SSID/password/IP are snapshotted once at show() — they are fixed for
 * the lifetime of a provisioning session and the page only ever ends via the
 * esp_restart() bb_wifi does after credentials are submitted, so nothing here
 * needs to update after creation; only the glyph animates.
 */
#include "bb_page_apconfig.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
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

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

static const char* TAG = "bb_page_apconfig";

/* ── palette — design/UI_DESIGN_LANGUAGE.md tokens ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_DOT_LIT   BB_UI_DOT_LIT
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_TEXT_DIM  BB_UI_TEXT_DIM
#define UI_ACCENT    BB_UI_ACCENT

/* ── WiFi glyph geometry — base dot + 3 concentric dot-arcs (netconn) ── */
#define MX_DOT       5
#define RING_COUNT   4 /* ring 0 = base dot */
#define RING_MAX_DOT 7
#define GLYPH_CX     46
#define GLYPH_BASE_Y 108 /* base-dot center, y; arcs fan upward from here */

typedef struct {
  int8_t dx;
  int8_t dy;
} ring_off_t;

static const ring_off_t RING0[] = {{0, 0}};
static const ring_off_t RING1[] = {{-8, -9}, {0, -12}, {8, -9}};
static const ring_off_t RING2[] = {{-16, -16}, {-8, -20}, {0, -22}, {8, -20}, {16, -16}};
static const ring_off_t RING3[] = {{-25, -21}, {-18, -27}, {-9, -31}, {0, -32}, {9, -31}, {18, -27}, {25, -21}};

static const ring_off_t* const RINGS[RING_COUNT] = {RING0, RING1, RING2, RING3};
static const int RING_DOTS[RING_COUNT] = {1, 3, 5, 7};

/* ── text block geometry (right of the glyph) ── */
#define TEXT_X    96
#define TITLE_Y   20
#define ROW0_Y    58
#define ROW_PITCH 36
#define BADGE_X   TEXT_X
#define DESC_X    (TEXT_X + 18)
#define VALUE_X   (TEXT_X + 60)

/* ── timing — slow outward ripple, "broadcasting / waiting to be joined" ── */
#define APCONFIG_TICK_MS 460
#define STEP_HOLD        RING_COUNT
#define STEP_RESET       (RING_COUNT + 1)

#ifdef BBCLAW_HAVE_CJK_FONT
#define TXT_TITLE "WiFi 配网"
#define TXT_DESC1 "热点"
#define TXT_DESC2 "密码"
#define TXT_DESC3 "打开"
#define TXT_OPEN  "开放网络"
#else
#define TXT_TITLE "WiFi SETUP"
#define TXT_DESC1 "SSID"
#define TXT_DESC2 "PWD"
#define TXT_DESC3 "OPEN"
#define TXT_OPEN  "OPEN NET"
#endif

static lv_obj_t* s_root;
static lv_obj_t* s_dots[RING_COUNT][RING_MAX_DOT];
static lv_timer_t* s_timer;
static int s_step;

static const lv_font_t* ui_font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

static void paint_ring(int ring, uint32_t color) {
  if (ring < 0 || ring >= RING_COUNT) return;
  for (int i = 0; i < RING_DOTS[ring]; i++) {
    lv_obj_set_style_bg_color(s_dots[ring][i], lv_color_hex(color), 0);
  }
}

/* Runs in the LVGL task — port mutex already held around lv_timer_handler. */
static void destroy_now(void) {
  if (s_timer != NULL) {
    lv_timer_del(s_timer);
    s_timer = NULL;
  }
  if (s_root != NULL) {
    lv_obj_del(s_root);
    s_root = NULL;
  }
}

static void apconfig_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_root == NULL) return;

  /* Outward ripple: light rings bottom→top, settle the previous ring to a
   * cool-white afterglow, then reset to ghost and loop. */
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

static lv_obj_t* make_label(const char* text, int x, int y, uint32_t color) {
  lv_obj_t* l = lv_label_create(s_root);
  lv_obj_set_style_text_color(l, lv_color_hex(color), 0);
  lv_obj_set_style_text_font(l, ui_font(), 0);
  lv_obj_set_pos(l, x, y);
  lv_label_set_text(l, text);
  return l;
}

static void make_row(int idx, const char* badge, const char* desc, const char* value) {
  int y = ROW0_Y + idx * ROW_PITCH;
  make_label(badge, BADGE_X, y, UI_ACCENT);
  make_label(desc, DESC_X, y, UI_TEXT_DIM);
  lv_obj_t* v = make_label(value, VALUE_X, y, UI_DOT_LIT);
  lv_obj_set_width(v, BBCLAW_ST7789_WIDTH - VALUE_X - 6);
  lv_label_set_long_mode(v, LV_LABEL_LONG_CLIP);
}

void bb_page_apconfig_show(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "show: lvgl lock timeout — skipping apconfig page");
    return;
  }
  if (s_root != NULL) {
    lvgl_port_unlock();
    return;
  }

  /* Snapshot AP info once — fixed for the provisioning session. */
  char ssid[64];
  char pwd[64];
  char ip[32];
  snprintf(ssid, sizeof(ssid), "%s", bb_wifi_get_ap_ssid());
  snprintf(ip, sizeof(ip), "%s", bb_wifi_get_ap_ip());
  const char* ap_pwd = bb_wifi_get_ap_password();
  if (ap_pwd != NULL && ap_pwd[0] != '\0') {
    snprintf(pwd, sizeof(pwd), "%s", ap_pwd);
  } else {
    snprintf(pwd, sizeof(pwd), "%s", TXT_OPEN);
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
      lv_obj_set_pos(d, GLYPH_CX + RINGS[r][i].dx - MX_DOT / 2, GLYPH_BASE_Y + RINGS[r][i].dy - MX_DOT / 2);
      lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
      lv_obj_set_style_bg_color(d, lv_color_hex(UI_DOT_GHOST), 0);
      lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
      lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
      s_dots[r][i] = d;
    }
  }

  /* Title + three-step join guide. */
  make_label(TXT_TITLE, TEXT_X, TITLE_Y, UI_ACCENT);
  make_row(0, "1", TXT_DESC1, ssid);
  make_row(1, "2", TXT_DESC2, pwd);
  make_row(2, "3", TXT_DESC3, ip);

  /* Light the base dot immediately so the first frame already reads as
   * "broadcasting" instead of an all-ghost beat. */
  paint_ring(0, UI_ACCENT);
  s_step = 1;
  s_timer = lv_timer_create(apconfig_timer_cb, APCONFIG_TICK_MS, NULL);

  lvgl_port_unlock();
  ESP_LOGI(TAG, "apconfig page shown ssid=%s ip=%s", ssid, ip);
}

void bb_page_apconfig_dismiss(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "dismiss: lvgl lock timeout — page stays");
    return;
  }
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "apconfig page dismissed");
}

int bb_page_apconfig_active(void) {
  return s_root != NULL ? 1 : 0;
}
