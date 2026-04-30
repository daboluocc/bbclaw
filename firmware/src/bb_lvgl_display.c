/**
 * ST7789 + LVGL display — redesigned two-zone layout.
 *
 * STANDBY: clock animation + brand icons (no text body).
 * All other states: top status bar + full-screen scrollable text area.
 */
#include "bb_display.h"
#include "bb_status.h"

#if defined(BBCLAW_SIMULATOR)
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_lvgl_element_assets.h"
#include "bb_lvgl_assets.h"
#include "bb_lvgl_img_elements.h"
#include "bb_time.h"
#include "bb_wifi.h"
#include "lvgl.h"

#define TAG "bb_lvgl_disp"
#define portMUX_TYPE int
#define portMUX_INITIALIZER_UNLOCKED 0
#define portENTER_CRITICAL(lock) ((void)(lock))
#define portEXIT_CRITICAL(lock) ((void)(lock))
#define pdMS_TO_TICKS(ms) (ms)
#define ESP_LOGI(tag, fmt, ...) ((void)(tag))
#define ESP_LOGW(tag, fmt, ...) ((void)(tag))
#define ESP_LOGE(tag, fmt, ...) ((void)(tag))
static int lvgl_port_lock(int timeout_ms) {
  (void)timeout_ms;
  return 1;
}
static void lvgl_port_unlock(void) {}
const char *bbclaw_session_key(void) { return "sim:preview"; }
#else
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_lvgl_element_assets.h"
#include "bb_lvgl_assets.h"
#include "bb_lvgl_img_elements.h"
#include "bb_panel.h"
#include "bb_time.h"
#include "bb_wifi.h"
#include "driver/gpio.h"
#include "esp_check.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "esp_lvgl_port_disp.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#endif

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

#if !defined(BBCLAW_SIMULATOR)
static const char* TAG = "bb_lvgl_disp";
#endif

#if defined(CONFIG_LV_FONT_MONTSERRAT_40) || defined(LV_FONT_MONTSERRAT_40)
LV_FONT_DECLARE(lv_font_montserrat_40)
#endif
#if defined(CONFIG_LV_FONT_MONTSERRAT_48) || defined(LV_FONT_MONTSERRAT_48)
LV_FONT_DECLARE(lv_font_montserrat_48)
#endif

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

/* Colors */
#define UI_SCR_BG        0x0a0e0c
#define UI_TEXT_MAIN     0xd8ebe4
#define UI_TEXT_DIM      0x7a9a8c
#define UI_STATUS_FG     0x8fbcac
#define UI_ME_ACCENT     0x2ec4a0
#define UI_AI_ACCENT     0x4a9fd8

/* Layout */
#define UI_SAFE_LEFT     10
#define UI_SAFE_RIGHT    12
#define UI_SAFE_TOP      8
#define UI_SAFE_BOTTOM   10
#define UI_GAP           2
#define UI_STATUS_ICON_SZ 16

/* Auto-scroll */
#define UI_AUTO_SCROLL_PERIOD_MS       96
#define UI_AUTO_SCROLL_STEP_PX         1
#define UI_AUTO_SCROLL_TOP_HOLD_TICKS  12
#define UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS 14
#define UI_MANUAL_SCROLL_STEP_LINES    2
#define UI_MANUAL_SCROLL_PAUSE_MS      4000

/* WiFi bars */
#define UI_WIFI_BAR_COUNT  4
#define UI_WIFI_BAR_W      3
#define UI_WIFI_BAR_GAP    2
#define UI_WIFI_BAR_H_STEP 4

/* Battery widget */
#define UI_BATTERY_W       44
#define UI_BATTERY_H       14
#define UI_BATTERY_FILL_W  18
#define UI_BATTERY_FILL_H  8

/* Recording speaking view */
#define UI_RECORD_UPDATE_MS       48
#define UI_RECORD_BAR_COUNT       10
#define UI_RECORD_BAR_W           10
#define UI_RECORD_BAR_GAP         4
#define UI_RECORD_BAR_MIN_H       6
#define UI_RECORD_BAR_MAX_H       38
#define UI_RECORD_HALO_BASE_PX    54
#define UI_RECORD_HALO_SPAN_PX    18
#define UI_RECORD_LEVEL_STALE_MS  280

/* Standby mascot (LimeZu idle — green shown; red/blue frames in bb_lvgl_element_assets for future use) */
#define UI_MASCOT_FRAME_MS        220
#define UI_MASCOT_PX              32

/* Panel config */
#define DISP_X_GAP         BBCLAW_ST7789_X_GAP
#define DISP_Y_GAP         BBCLAW_ST7789_Y_GAP
#define DISP_PCLK_HZ       BBCLAW_ST7789_PCLK_HZ
#define DISP_SWAP_XY        BBCLAW_ST7789_SWAP_XY
#define DISP_MIRROR_X       BBCLAW_ST7789_MIRROR_X
#define DISP_MIRROR_Y       BBCLAW_ST7789_MIRROR_Y
#define DISP_SWAP_BYTES     BBCLAW_ST7789_SWAP_BYTES
#define DISP_INVERT_COLOR   BBCLAW_ST7789_INVERT_COLOR
#if BBCLAW_ST7789_RGB_ORDER_BGR
#define DISP_RGB_ORDER      LCD_RGB_ELEMENT_ORDER_BGR
#define DISP_RGB_ORDER_NAME "BGR"
#else
#define DISP_RGB_ORDER      LCD_RGB_ELEMENT_ORDER_RGB
#define DISP_RGB_ORDER_NAME "RGB"
#endif

/* Decor scale for brand icons */
#define UI_DECOR_SCALE_HERO   72

typedef struct {
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
} bb_chat_turn_t;

typedef enum {
  UI_VIEW_STANDBY = 0,
  UI_VIEW_LOCKED,
  UI_VIEW_ACTIVE,
} ui_view_mode_t;

typedef enum {
  UI_AUTO_SCROLL_HOLD_TOP = 0,
  UI_AUTO_SCROLL_RUNNING,
  UI_AUTO_SCROLL_HOLD_BOTTOM,
  UI_AUTO_SCROLL_IDLE,  /* 滚动到底后停止，等待用户手动滚动或新内容 */
} ui_auto_scroll_phase_t;

typedef struct {
  lv_obj_t* cont;
  ui_auto_scroll_phase_t phase;
  uint16_t wait_ticks;
} ui_auto_scroll_ctx_t;

#if !defined(BBCLAW_SIMULATOR)
static esp_lcd_panel_io_handle_t s_panel_io;
static esp_lcd_panel_handle_t s_panel;
#endif
static portMUX_TYPE s_state_lock = portMUX_INITIALIZER_UNLOCKED;

/* State */
static bb_chat_turn_t s_history[BBCLAW_DISPLAY_CHAT_HISTORY];
static int s_history_count;
static int s_stream_turn_active;
static int s_view_back;
static int s_scroll_you;
static int s_scroll_ai;
static int s_focus_ai;
static char s_status[32];
static int64_t s_last_active_ms;  /* last non-idle activity timestamp */

/* LVGL objects — standby */
static lv_obj_t* s_view_standby;
static lv_obj_t* s_img_standby_brand_claw;
static lv_obj_t* s_img_standby_brand_openclaw;
static lv_obj_t* s_lbl_standby_brand_join;
static lv_obj_t* s_lbl_standby_clock;
static lv_obj_t* s_lbl_standby_session;
static lv_obj_t* s_lbl_standby_hint;
static lv_obj_t* s_img_standby_mascot;

/* LVGL objects — locked */
static lv_obj_t* s_view_locked;
static lv_obj_t* s_obj_locked_shackle;
static lv_obj_t* s_obj_locked_body;
static lv_obj_t* s_obj_locked_slot;
static lv_obj_t* s_lbl_locked_title;
static lv_obj_t* s_lbl_locked_hint;

/* LVGL objects — active (status bar + text) */
static lv_obj_t* s_view_active;
static lv_obj_t* s_img_mode;      /* HOME/CLOUD mode indicator */
static lv_obj_t* s_img_status;
static lv_obj_t* s_lbl_status;
static lv_obj_t* s_lbl_status_clock;
static lv_obj_t* s_obj_status_wifi;
static lv_obj_t* s_lbl_status_wifi_info;
static lv_obj_t* s_bar_status_wifi[UI_WIFI_BAR_COUNT];
static lv_obj_t* s_obj_status_battery;
static lv_obj_t* s_obj_status_battery_fill;
static lv_obj_t* s_img_status_battery;
static lv_obj_t* s_lbl_status_battery;
static lv_obj_t* s_view_speaking;
static lv_obj_t* s_obj_record_halo_outer;
static lv_obj_t* s_obj_record_halo_inner;
static lv_obj_t* s_obj_record_badge;
static lv_obj_t* s_img_record_badge;
static lv_obj_t* s_lbl_record_title;
static lv_obj_t* s_lbl_record_state;
static lv_obj_t* s_lbl_record_hint;
static lv_obj_t* s_obj_record_meter;
static lv_obj_t* s_obj_record_bar[UI_RECORD_BAR_COUNT];
static lv_obj_t* s_scroll_text;
static lv_obj_t* s_lbl_text;

/* Timers */
static lv_timer_t* s_clock_timer;
static lv_timer_t* s_auto_scroll_timer;
static lv_timer_t* s_record_timer;
static lv_timer_t* s_mascot_timer;

/* Scroll state */
static ui_auto_scroll_ctx_t s_auto_scroll_text;
static int64_t s_auto_scroll_pause_until_ms;

static int s_ready;
static int s_locked;
static int s_main_text_scroll_dirty;
static int s_main_text_scroll_to_bottom;
static int s_tts_playing;
static int s_last_visible_mode = -1;
static uint8_t s_record_level_pct;
static int s_record_voiced;
static int64_t s_record_level_updated_ms;
static uint8_t s_record_bar_visual[UI_RECORD_BAR_COUNT];
static uint32_t s_record_anim_tick;
static int s_record_view_visible;
static uint32_t s_mascot_frame;
static int s_battery_available;
static int s_battery_percent = -1;
static int s_battery_low;
static int s_battery_supported;
static int s_cloud_mode;  /* 1 = cloud_saas, 0 = local_home */

static void refresh_ui(void);

/* ── Fonts ── */

static const lv_font_t* ui_font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

static const lv_font_t* ui_font_clock(void) {
#if defined(CONFIG_LV_FONT_MONTSERRAT_48) || defined(LV_FONT_MONTSERRAT_48)
  return &lv_font_montserrat_48;
#elif defined(CONFIG_LV_FONT_MONTSERRAT_40) || defined(LV_FONT_MONTSERRAT_40)
  return &lv_font_montserrat_40;
#else
  return ui_font();
#endif
}

static int line_px(void) {
  return (int)lv_font_get_line_height(ui_font()) + 1;
}

static lv_coord_t text_width_px(const char* text, const lv_font_t* font) {
  lv_point_t size = {0};
  if (text == NULL || font == NULL) return 0;
  lv_text_get_size(&size, text, font, 0, 0, LV_COORD_MAX, LV_TEXT_FLAG_NONE);
  return size.x;
}

/* ── WiFi helpers ── */

static void format_wifi_info(char* out, size_t out_size) {
  const char* ssid = bb_wifi_get_active_ssid();
  if (ssid != NULL && ssid[0] != '\0') {
    snprintf(out, out_size, "%s", ssid);
  } else {
    snprintf(out, out_size, "WiFi");
  }
}

static int wifi_signal_level(const char* status) {
  if (status != NULL && strstr(status, BB_STATUS_ERR) != NULL) return 0;
  int rssi = bb_wifi_get_rssi();
  if (rssi == 0) return 1;
  if (rssi >= -50) return 4;
  if (rssi >= -65) return 3;
  if (rssi >= -80) return 2;
  return 1;
}

static void apply_wifi_bars(lv_obj_t* bars[], lv_obj_t* info_lbl, const char* status) {
  if (info_lbl == NULL) return;
  const int level = wifi_signal_level(status);
  lv_color_t on = lv_color_hex(UI_ME_ACCENT);
  lv_color_t off = lv_color_hex(UI_TEXT_DIM);
  for (int i = 0; i < UI_WIFI_BAR_COUNT; ++i) {
    if (bars[i] == NULL) continue;
    lv_obj_set_style_bg_color(bars[i], i < level ? on : off, 0);
    lv_obj_set_style_bg_opa(bars[i], i < level ? LV_OPA_COVER : LV_OPA_50, 0);
  }
  char wifi_info[64];
  format_wifi_info(wifi_info, sizeof(wifi_info));
  lv_label_set_text(info_lbl, wifi_info);
}

static void apply_battery_widget(void) {
  if (s_obj_status_battery == NULL || s_obj_status_battery_fill == NULL || s_lbl_status_battery == NULL) return;

  int supported = 0;
  int available = 0;
  int percent = -1;
  int low = 0;
  portENTER_CRITICAL(&s_state_lock);
  supported = s_battery_supported;
  available = s_battery_available;
  percent = s_battery_percent;
  low = s_battery_low;
  portEXIT_CRITICAL(&s_state_lock);

  if (!supported) {
    lv_obj_add_flag(s_obj_status_battery, LV_OBJ_FLAG_HIDDEN);
    return;
  }

  lv_obj_clear_flag(s_obj_status_battery, LV_OBJ_FLAG_HIDDEN);
  if (!available || percent < 0) {
    lv_obj_add_flag(s_obj_status_battery_fill, LV_OBJ_FLAG_HIDDEN);
    lv_label_set_text(s_lbl_status_battery, "--");
    return;
  }

  if (percent > 100) percent = 100;
  if (percent < 0) percent = 0;
  lv_obj_clear_flag(s_obj_status_battery_fill, LV_OBJ_FLAG_HIDDEN);
  lv_obj_set_width(s_obj_status_battery_fill, (percent * UI_BATTERY_FILL_W) / 100);
  lv_obj_set_style_bg_color(s_obj_status_battery_fill, lv_color_hex(low ? 0xe66f6f : UI_ME_ACCENT), 0);

  char pct[8];
  snprintf(pct, sizeof(pct), "%d", percent);
  lv_label_set_text(s_lbl_status_battery, pct);
}

/* ── Status icon ── */

static void apply_status_icon(const char* status) {
  if (s_img_status == NULL) return;
  const lv_image_dsc_t* src = &bb_img_ready;
  if (status == NULL || status[0] == '\0') {
    lv_image_set_src(s_img_status, src);
    return;
  }
  if (strstr(status, BB_STATUS_ERR) != NULL) src = &bb_img_err;
  else if (strcmp(status, BB_STATUS_TX) == 0) src = &bb_img_tx;
  else if (strcmp(status, BB_STATUS_RX) == 0) src = &bb_img_rx;
  else if (strcmp(status, BB_STATUS_TASK) == 0 || strcmp(status, BB_STATUS_BUSY) == 0) src = &bb_img_task;
  else if (strcmp(status, BB_STATUS_SPEAK) == 0) src = &bb_img_speak;
  else if (strcmp(status, BB_STATUS_RESULT) == 0) src = &bb_img_ready;
  else if (strncmp(status, BB_STATUS_BOOT, 4) == 0 || strstr(status, BB_STATUS_WIFI) != NULL ||
           strstr(status, BB_STATUS_ADAPTER) != NULL || strstr(status, BB_STATUS_SPK) != NULL) src = &bb_img_task;
  else if (strcmp(status, BB_STATUS_READY) == 0) src = &bb_img_ready;
  lv_image_set_src(s_img_status, src);
}

static int is_recording_status(const char* status) {
  return status != NULL && strcmp(status, BB_STATUS_TX) == 0;
}

static const lv_image_dsc_t* record_anim_icon(uint32_t tick) {
  switch (tick % 3U) {
    case 0:
      return &bb_img_rec_1;
    case 1:
      return &bb_img_rec_2;
    default:
      return &bb_img_rec_3;
  }
}

static const lv_image_dsc_t* standby_mascot_green_frame(uint32_t tick) {
  switch (tick % 4U) {
    case 0:
      return &bb_el_green_idle_0;
    case 1:
      return &bb_el_green_idle_1;
    case 2:
      return &bb_el_green_idle_2;
    default:
      return &bb_el_green_idle_3;
  }
}

/* ── View mode ── */

static int is_standby_status(const char* status) {
  return status == NULL || status[0] == '\0' || strcmp(status, BB_STATUS_READY) == 0;
}

static int should_show_locked_view(int locked, const char* status) {
  if (!locked) return 0;
  if (status == NULL || status[0] == '\0') return 1;
  if (strcmp(status, BB_STATUS_LOCKED) == 0 || strcmp(status, BB_STATUS_READY) == 0) return 1;
  if (strncmp(status, BB_STATUS_VERIFY, 6) == 0) return 1;
  return 0;
}

static ui_view_mode_t resolve_view_mode(const char* status, int turn_den, int locked) {
  if (should_show_locked_view(locked, status)) return UI_VIEW_LOCKED;
  /* If there are chat turns to show, or status is not idle, go active */
  if (!is_standby_status(status)) return UI_VIEW_ACTIVE;
  if (turn_den > 0) return UI_VIEW_ACTIVE;
  return UI_VIEW_STANDBY;
}

/* ── Clock ── */

static void format_clock(char* out, size_t out_size, int64_t now_ms) {
  if (out == NULL || out_size == 0) return;
  bb_wall_time_format_hm(out, out_size);
  if (((now_ms / 1000) & 1LL) != 0 && strlen(out) >= 3 && out[2] == ':') {
    out[2] = ' ';
  }
}

static void standby_clock_anim_y_cb(void* obj, int32_t v) {
  lv_obj_set_y((lv_obj_t*)obj, (lv_coord_t)v);
}

/* ── Auto-scroll ── */

static void scroll_cont_reset_top(lv_obj_t* cont) {
  if (cont != NULL) lv_obj_scroll_to_y(cont, 0, LV_ANIM_OFF);
}

static void auto_scroll_ctx_attach(ui_auto_scroll_ctx_t* ctx, lv_obj_t* cont) {
  if (ctx == NULL) return;
  ctx->cont = cont;
  ctx->phase = UI_AUTO_SCROLL_HOLD_TOP;
  ctx->wait_ticks = UI_AUTO_SCROLL_TOP_HOLD_TICKS;
}

static void auto_scroll_ctx_reset(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL) return;
  ctx->phase = UI_AUTO_SCROLL_HOLD_TOP;
  ctx->wait_ticks = UI_AUTO_SCROLL_TOP_HOLD_TICKS;
  scroll_cont_reset_top(ctx->cont);
}

static void auto_scroll_ctx_note_manual(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL || ctx->cont == NULL) return;
  lv_obj_update_layout(ctx->cont);
  int32_t y = lv_obj_get_scroll_y(ctx->cont);
  int32_t max_y = lv_obj_get_scroll_bottom(ctx->cont);
  if (max_y <= UI_AUTO_SCROLL_STEP_PX || y <= 0) {
    ctx->phase = UI_AUTO_SCROLL_HOLD_TOP;
    ctx->wait_ticks = UI_AUTO_SCROLL_TOP_HOLD_TICKS;
    return;
  }
  if (y >= max_y - UI_AUTO_SCROLL_STEP_PX) {
    ctx->phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
    ctx->wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
    return;
  }
  ctx->phase = UI_AUTO_SCROLL_RUNNING;
  ctx->wait_ticks = 0;
}

static int scroll_cont_chain_visible(const lv_obj_t* cont) {
  for (const lv_obj_t* p = cont; p != NULL; p = lv_obj_get_parent(p)) {
    if (lv_obj_has_flag(p, LV_OBJ_FLAG_HIDDEN)) return 0;
  }
  return 1;
}

static void set_record_bar_height(lv_obj_t* bar, int height) {
  if (bar == NULL) return;
  if (height < UI_RECORD_BAR_MIN_H) {
    height = UI_RECORD_BAR_MIN_H;
  } else if (height > UI_RECORD_BAR_MAX_H) {
    height = UI_RECORD_BAR_MAX_H;
  }
  lv_obj_set_size(bar, UI_RECORD_BAR_W, height);
  lv_obj_set_y(bar, UI_RECORD_BAR_MAX_H - height);
}

static void reset_recording_meter_visuals(void) {
  s_record_anim_tick = 0;
  for (int i = 0; i < UI_RECORD_BAR_COUNT; ++i) {
    s_record_bar_visual[i] = UI_RECORD_BAR_MIN_H;
    set_record_bar_height(s_obj_record_bar[i], UI_RECORD_BAR_MIN_H);
  }
  if (s_obj_record_halo_outer != NULL) {
    lv_obj_set_size(s_obj_record_halo_outer, UI_RECORD_HALO_BASE_PX + 14, UI_RECORD_HALO_BASE_PX + 14);
    lv_obj_set_style_bg_opa(s_obj_record_halo_outer, LV_OPA_0, 0);
  }
  if (s_obj_record_halo_inner != NULL) {
    lv_obj_set_size(s_obj_record_halo_inner, UI_RECORD_HALO_BASE_PX, UI_RECORD_HALO_BASE_PX);
    lv_obj_set_style_bg_opa(s_obj_record_halo_inner, LV_OPA_0, 0);
  }
  if (s_obj_record_badge != NULL) {
    lv_obj_set_style_bg_color(s_obj_record_badge, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_bg_opa(s_obj_record_badge, LV_OPA_20, 0);
    lv_obj_set_style_border_color(s_obj_record_badge, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_border_opa(s_obj_record_badge, LV_OPA_40, 0);
  }
  if (s_img_record_badge != NULL) {
    lv_image_set_src(s_img_record_badge, &bb_img_tx);
  }
  if (s_lbl_record_state != NULL) {
    lv_label_set_text(s_lbl_record_state, "请靠近麦克风说话");
  }
}

static void refresh_recording_meter(void) {
  if (s_view_speaking == NULL || !scroll_cont_chain_visible(s_view_speaking)) {
    return;
  }

  static const uint8_t kProfiles[UI_RECORD_BAR_COUNT] = {32, 46, 64, 82, 100, 100, 82, 64, 46, 32};

  uint8_t level_pct = 0;
  int voiced = 0;
  int64_t updated_ms = 0;
  portENTER_CRITICAL(&s_state_lock);
  level_pct = s_record_level_pct;
  voiced = s_record_voiced;
  updated_ms = s_record_level_updated_ms;
  portEXIT_CRITICAL(&s_state_lock);

  const int64_t now_ms = bb_now_ms();
  if (updated_ms == 0 || (now_ms - updated_ms) > UI_RECORD_LEVEL_STALE_MS) {
    level_pct = 0;
    voiced = 0;
  }

  s_record_anim_tick++;
  for (int i = 0; i < UI_RECORD_BAR_COUNT; ++i) {
    int wobble = 0;
    if (level_pct > 3U) {
      wobble = (int)((s_record_anim_tick + (uint32_t)(i * 3)) % 7U) - 3;
    }
    int target_h = UI_RECORD_BAR_MIN_H + (int)((level_pct * (uint32_t)kProfiles[i] *
                                                (UI_RECORD_BAR_MAX_H - UI_RECORD_BAR_MIN_H)) /
                                               10000U);
    target_h += wobble;
    if (target_h < UI_RECORD_BAR_MIN_H) {
      target_h = UI_RECORD_BAR_MIN_H;
    } else if (target_h > UI_RECORD_BAR_MAX_H) {
      target_h = UI_RECORD_BAR_MAX_H;
    }

    int current_h = (int)s_record_bar_visual[i];
    if (target_h > current_h) {
      current_h += (target_h - current_h + 1) / 2;
    } else if (target_h < current_h) {
      current_h -= (current_h - target_h + 2) / 3;
    }
    s_record_bar_visual[i] = (uint8_t)current_h;
    set_record_bar_height(s_obj_record_bar[i], current_h);
    lv_obj_set_style_bg_opa(s_obj_record_bar[i], voiced ? LV_OPA_COVER : LV_OPA_70, 0);
  }

  if (s_obj_record_halo_outer != NULL) {
    int outer_size = UI_RECORD_HALO_BASE_PX + 14 + (int)((level_pct * (UI_RECORD_HALO_SPAN_PX + 6)) / 100U);
    lv_obj_set_size(s_obj_record_halo_outer, outer_size, outer_size);
    lv_obj_set_style_bg_opa(s_obj_record_halo_outer, voiced ? (lv_opa_t)(10 + (level_pct * 28U) / 100U) : (lv_opa_t)6, 0);
  }
  if (s_obj_record_halo_inner != NULL) {
    int inner_size = UI_RECORD_HALO_BASE_PX + (int)((level_pct * UI_RECORD_HALO_SPAN_PX) / 100U);
    lv_obj_set_size(s_obj_record_halo_inner, inner_size, inner_size);
    lv_obj_set_style_bg_opa(s_obj_record_halo_inner, voiced ? (lv_opa_t)(16 + (level_pct * 40U) / 100U) : LV_OPA_10, 0);
  }
  if (s_obj_record_badge != NULL) {
    lv_obj_set_style_bg_color(s_obj_record_badge, lv_color_hex(voiced ? UI_ME_ACCENT : UI_TEXT_DIM), 0);
    lv_obj_set_style_bg_opa(s_obj_record_badge, voiced ? (lv_opa_t)(48 + (level_pct * 52U) / 100U) : LV_OPA_20, 0);
    lv_obj_set_style_border_color(s_obj_record_badge, lv_color_hex(voiced ? UI_ME_ACCENT : UI_TEXT_DIM), 0);
    lv_obj_set_style_border_opa(s_obj_record_badge, voiced ? LV_OPA_COVER : LV_OPA_40, 0);
  }
  if (s_img_record_badge != NULL) {
    lv_image_set_src(s_img_record_badge, voiced ? record_anim_icon(s_record_anim_tick) : &bb_img_tx);
  }
  if (s_img_status != NULL) {
    lv_image_set_src(s_img_status, voiced ? record_anim_icon(s_record_anim_tick) : &bb_img_tx);
  }
  if (s_lbl_record_state != NULL) {
    lv_label_set_text(s_lbl_record_state, voiced ? "已检测到声音" : "请靠近麦克风说话");
  }
}

static void auto_scroll_step_ctx(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL || ctx->cont == NULL || !scroll_cont_chain_visible(ctx->cont)) return;
  lv_obj_update_layout(ctx->cont);
  int32_t max_y = lv_obj_get_scroll_bottom(ctx->cont);
  if (max_y <= UI_AUTO_SCROLL_STEP_PX) {
    auto_scroll_ctx_reset(ctx);
    return;
  }
  int32_t y = lv_obj_get_scroll_y(ctx->cont);
  switch (ctx->phase) {
    case UI_AUTO_SCROLL_HOLD_TOP:
      if (y != 0) lv_obj_scroll_to_y(ctx->cont, 0, LV_ANIM_OFF);
      if (ctx->wait_ticks > 0) ctx->wait_ticks--;
      else ctx->phase = UI_AUTO_SCROLL_RUNNING;
      break;
    case UI_AUTO_SCROLL_HOLD_BOTTOM:
      if (y < max_y) lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
      if (ctx->wait_ticks > 0) ctx->wait_ticks--;
      else if (s_tts_playing) ctx->wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
      else {
        // TTS 结束，切换到 IDLE 状态，不再自动滚动
        ctx->phase = UI_AUTO_SCROLL_IDLE;
      }
      break;
    case UI_AUTO_SCROLL_IDLE:
      // 停在底部，等待用户手动滚动或新内容到达后通过 auto_scroll_ctx_reset 重置
      if (y < max_y) lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
      break;
    case UI_AUTO_SCROLL_RUNNING:
    default:
      if (y >= max_y - UI_AUTO_SCROLL_STEP_PX) {
        lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
        ctx->phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
        ctx->wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
      } else {
        lv_obj_scroll_to_y(ctx->cont, y + UI_AUTO_SCROLL_STEP_PX, LV_ANIM_OFF);
      }
      break;
  }
}

static void auto_scroll_text_cb(lv_timer_t* t) {
  (void)t;
  if (!s_ready) return;
  if (bb_now_ms() < s_auto_scroll_pause_until_ms) return;
  auto_scroll_step_ctx(&s_auto_scroll_text);
}

static void record_timer_cb(lv_timer_t* t) {
  (void)t;
  if (!s_ready) return;
  if (!lvgl_port_lock(0)) return;
  refresh_recording_meter();
  lvgl_port_unlock();
}

static void mascot_timer_cb(lv_timer_t* t) {
  (void)t;
  if (!s_ready) return;
  if (s_img_standby_mascot == NULL || s_view_standby == NULL) return;
  if (lv_obj_has_flag(s_view_standby, LV_OBJ_FLAG_HIDDEN)) return;
  if (!lvgl_port_lock(0)) return;
  s_mascot_frame++;
  lv_image_set_src(s_img_standby_mascot, standby_mascot_green_frame(s_mascot_frame));
  lvgl_port_unlock();
}

static void clock_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_ready) refresh_ui();
}

/* ── View visibility ── */

static void set_view_visible(lv_obj_t* obj, int visible) {
  if (obj == NULL) return;
  if (visible) lv_obj_clear_flag(obj, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(obj, LV_OBJ_FLAG_HIDDEN);
}

/* ── WiFi bar widget creation helper ── */

static lv_obj_t* create_wifi_widget(lv_obj_t* parent, int x, int y, lv_obj_t* bars[], lv_obj_t** info_lbl, int info_w) {
  const lv_font_t* font = ui_font();
  lv_obj_t* container = lv_obj_create(parent);
  lv_obj_remove_style_all(container);
  lv_obj_set_size(container, info_w, 16);
  lv_obj_set_pos(container, x, y);
  lv_obj_clear_flag(container, LV_OBJ_FLAG_SCROLLABLE);

  const int bars_total_w = UI_WIFI_BAR_COUNT * UI_WIFI_BAR_W + (UI_WIFI_BAR_COUNT - 1) * UI_WIFI_BAR_GAP;
  const int bars_x = info_w - bars_total_w;
  for (int i = 0; i < UI_WIFI_BAR_COUNT; ++i) {
    const int bar_h = (i + 1) * UI_WIFI_BAR_H_STEP;
    bars[i] = lv_obj_create(container);
    lv_obj_remove_style_all(bars[i]);
    lv_obj_set_size(bars[i], UI_WIFI_BAR_W, bar_h);
    lv_obj_set_style_radius(bars[i], 1, 0);
    lv_obj_set_pos(bars[i], bars_x + i * (UI_WIFI_BAR_W + UI_WIFI_BAR_GAP), 12 - bar_h);
  }

  *info_lbl = lv_label_create(container);
  lv_obj_set_width(*info_lbl, info_w - bars_total_w - 6);
  lv_obj_set_style_text_color(*info_lbl, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_text_font(*info_lbl, font, 0);
  lv_obj_set_style_text_align(*info_lbl, LV_TEXT_ALIGN_LEFT, 0);
  lv_label_set_long_mode(*info_lbl, LV_LABEL_LONG_MODE_SCROLL_CIRCULAR);
  lv_label_set_text(*info_lbl, "WiFi");
  lv_obj_set_pos(*info_lbl, 0, 0);

  return container;
}

static lv_obj_t* create_battery_widget(lv_obj_t* parent, int x, int y) {
  const lv_font_t* font = ui_font();
  lv_obj_t* container = lv_obj_create(parent);
  lv_obj_remove_style_all(container);
  lv_obj_set_size(container, UI_BATTERY_W, UI_BATTERY_H);
  lv_obj_set_pos(container, x, y);
  lv_obj_clear_flag(container, LV_OBJ_FLAG_SCROLLABLE);

  s_obj_status_battery_fill = lv_obj_create(container);
  lv_obj_remove_style_all(s_obj_status_battery_fill);
  lv_obj_set_size(s_obj_status_battery_fill, UI_BATTERY_FILL_W, UI_BATTERY_FILL_H);
  lv_obj_set_pos(s_obj_status_battery_fill, 2, 3);
  lv_obj_set_style_radius(s_obj_status_battery_fill, 1, 0);
  lv_obj_set_style_bg_color(s_obj_status_battery_fill, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_obj_status_battery_fill, LV_OPA_COVER, 0);

  s_img_status_battery = lv_image_create(container);
  lv_image_set_src(s_img_status_battery, &bb_el_battery_frame_26x12);
  lv_obj_set_pos(s_img_status_battery, 0, 1);

  s_lbl_status_battery = lv_label_create(container);
  lv_obj_set_width(s_lbl_status_battery, 16);
  lv_obj_set_style_text_color(s_lbl_status_battery, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_text_font(s_lbl_status_battery, font, 0);
  lv_obj_set_style_text_align(s_lbl_status_battery, LV_TEXT_ALIGN_RIGHT, 0);
  lv_label_set_long_mode(s_lbl_status_battery, LV_LABEL_LONG_MODE_CLIP);
  lv_label_set_text(s_lbl_status_battery, "--");
  lv_obj_set_pos(s_lbl_status_battery, 28, 0);

  return container;
}

/* ── Panel init (hardware only) ── */

#if !defined(BBCLAW_SIMULATOR)
static void backlight_on(void) {
#if BBCLAW_ST7789_BL_GPIO >= 0
  gpio_config_t io_conf = {
      .pin_bit_mask = 1ULL << BBCLAW_ST7789_BL_GPIO,
      .mode = GPIO_MODE_OUTPUT,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  (void)gpio_config(&io_conf);
  (void)gpio_set_level(BBCLAW_ST7789_BL_GPIO, 1);
#endif
}

static esp_err_t init_panel(void) {
  return bb_panel_init(&s_panel_io, &s_panel);
}

static void lvgl_flush_cb(lv_display_t* disp, const lv_area_t* area, uint8_t* color_map) {
  if (disp == NULL || area == NULL || color_map == NULL) {
    if (disp != NULL) lvgl_port_flush_ready(disp);
    return;
  }
#if DISP_SWAP_BYTES
  lv_draw_sw_rgb565_swap(color_map, lv_area_get_size(area));
#endif
  esp_err_t e = esp_lcd_panel_draw_bitmap(s_panel, area->x1, area->y1, area->x2 + 1, area->y2 + 1, color_map);
  if (e != ESP_OK) {
    ESP_LOGW(TAG, "draw failed: %s", esp_err_to_name(e));
    lvgl_port_flush_ready(disp);
  }
}
#endif /* !BBCLAW_SIMULATOR */

/* ── create_ui ── */

static void create_ui(void) {
  lv_obj_t* scr = lv_screen_active();
  lv_obj_set_style_bg_color(scr, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(scr, LV_OPA_COVER, 0);
  lv_obj_clear_flag(scr, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(scr, LV_SCROLLBAR_MODE_OFF);

  const lv_font_t* font = ui_font();
  const int lh = (int)lv_font_get_line_height(font);
  const int body_w = DISP_W - UI_SAFE_LEFT - UI_SAFE_RIGHT;
  const int status_h = (lh + 2 > UI_STATUS_ICON_SZ + 2) ? (lh + 2) : (UI_STATUS_ICON_SZ + 2);
  const int content_y = UI_SAFE_TOP + status_h + UI_GAP;
  const int content_h = DISP_H - content_y - UI_SAFE_BOTTOM;
  const int standby_clock_h = (int)lv_font_get_line_height(ui_font_clock());

  /* ── STANDBY view: brand icons + clock ── */

  s_view_standby = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view_standby);
  lv_obj_set_size(s_view_standby, DISP_W, DISP_H);
  lv_obj_set_pos(s_view_standby, 0, 0);
  lv_obj_clear_flag(s_view_standby, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_standby, LV_SCROLLBAR_MODE_OFF);

  /* Brand: [claw] x [openclaw] centered */
  {
    const lv_coord_t logo_w = (lv_coord_t)bb_img_logo_claw.header.w;
#if defined(BBCLAW_SIMULATOR)
    const lv_image_dsc_t* openclaw_logo = &bb_img_logo_openclaw;
#else
    const lv_image_dsc_t* openclaw_logo = &bb_img_logo_openclaw_panel;
#endif
    const lv_coord_t openclaw_w = (lv_coord_t)openclaw_logo->header.w;
    const lv_coord_t join_w = text_width_px("x", font);
    const lv_coord_t brand_gap = 4;
    const lv_coord_t brand_total = logo_w + brand_gap + join_w + brand_gap + openclaw_w;
    const lv_coord_t brand_x = (DISP_W - brand_total) / 2;
    const lv_coord_t brand_y = 20;

    s_img_standby_brand_claw = lv_image_create(s_view_standby);
    lv_image_set_src(s_img_standby_brand_claw, &bb_img_logo_claw);
    lv_obj_set_pos(s_img_standby_brand_claw, brand_x, brand_y);
    lv_obj_set_style_opa(s_img_standby_brand_claw, LV_OPA_80, 0);

    s_lbl_standby_brand_join = lv_label_create(s_view_standby);
    lv_obj_set_style_text_color(s_lbl_standby_brand_join, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_standby_brand_join, font, 0);
    lv_label_set_text(s_lbl_standby_brand_join, "x");
    lv_obj_set_pos(s_lbl_standby_brand_join, brand_x + logo_w + brand_gap, brand_y + 2);

    s_img_standby_brand_openclaw = lv_image_create(s_view_standby);
    lv_image_set_src(s_img_standby_brand_openclaw, openclaw_logo);
    lv_obj_set_pos(s_img_standby_brand_openclaw, brand_x + logo_w + brand_gap + join_w + brand_gap, brand_y);
    lv_obj_set_style_opa(s_img_standby_brand_openclaw, LV_OPA_90, 0);
  }

  /* Clock with float animation */
  {
    const lv_coord_t clock_w = DISP_W - 40;
    const int clock_y = 56;

    s_lbl_standby_clock = lv_label_create(s_view_standby);
    lv_obj_set_width(s_lbl_standby_clock, clock_w);
    lv_obj_set_height(s_lbl_standby_clock, standby_clock_h + 2);
    lv_obj_set_style_text_color(s_lbl_standby_clock, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_text_font(s_lbl_standby_clock, ui_font_clock(), 0);
    lv_obj_set_style_text_align(s_lbl_standby_clock, LV_TEXT_ALIGN_CENTER, 0);
    lv_obj_set_style_text_letter_space(s_lbl_standby_clock, 1, 0);
    lv_label_set_long_mode(s_lbl_standby_clock, LV_LABEL_LONG_MODE_CLIP);
    lv_label_set_text(s_lbl_standby_clock, "--:--");
    lv_obj_set_pos(s_lbl_standby_clock, (DISP_W - clock_w) / 2, clock_y);

    lv_anim_t a;
    lv_anim_init(&a);
    lv_anim_set_var(&a, s_lbl_standby_clock);
    lv_anim_set_values(&a, clock_y, clock_y - 3);
    lv_anim_set_duration(&a, 1200);
    lv_anim_set_playback_duration(&a, 1200);
    lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
    lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
    lv_anim_set_exec_cb(&a, standby_clock_anim_y_cb);
    lv_anim_start(&a);
  }

  /* Standby bottom (ADR-012 §5): session/pairing line + key-hint bar.
   *
   * Layout from bottom up:
   *   row -1 (very bottom):  "[OK]设置  [BACK]聊天" hint, centered
   *   row -2:                 session/pairing label, left-aligned
   *
   * The decorative "BBClaw" title was removed — the hint bar carries
   * functional information that didn't fit before. */
  {
    s_lbl_standby_session = lv_label_create(s_view_standby);
    lv_obj_set_width(s_lbl_standby_session, DISP_W - UI_SAFE_LEFT - UI_SAFE_RIGHT - 8);
    lv_obj_set_style_text_color(s_lbl_standby_session, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_standby_session, font, 0);
    lv_obj_set_style_text_opa(s_lbl_standby_session, LV_OPA_60, 0);
    lv_label_set_long_mode(s_lbl_standby_session, LV_LABEL_LONG_MODE_CLIP);
    lv_label_set_text(s_lbl_standby_session, BBCLAW_SESSION_KEY);
    lv_obj_set_pos(s_lbl_standby_session, UI_SAFE_LEFT + 4, DISP_H - UI_SAFE_BOTTOM - lh * 2 - 4);

    s_lbl_standby_hint = lv_label_create(s_view_standby);
    lv_obj_set_width(s_lbl_standby_hint, DISP_W - UI_SAFE_LEFT - UI_SAFE_RIGHT);
    lv_obj_set_style_text_color(s_lbl_standby_hint, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_standby_hint, font, 0);
    lv_obj_set_style_text_align(s_lbl_standby_hint, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(s_lbl_standby_hint, LV_LABEL_LONG_MODE_CLIP);
    lv_label_set_text(s_lbl_standby_hint, "[OK]设置  [BACK]聊天");
    lv_obj_set_pos(s_lbl_standby_hint, UI_SAFE_LEFT, DISP_H - UI_SAFE_BOTTOM - lh - 2);
  }

  /* Standby corner mascot (LimeZu green idle loop; red/blue: bb_el_red_idle_* / bb_el_blue_idle_*) */
  {
    s_mascot_frame = 0;
    s_img_standby_mascot = lv_image_create(s_view_standby);
    lv_image_set_src(s_img_standby_mascot, standby_mascot_green_frame(0));
    lv_obj_set_size(s_img_standby_mascot, UI_MASCOT_PX, UI_MASCOT_PX);
    lv_obj_set_pos(s_img_standby_mascot, DISP_W - UI_SAFE_RIGHT - UI_MASCOT_PX,
                   DISP_H - UI_SAFE_BOTTOM - UI_MASCOT_PX - 2);
    lv_obj_set_style_opa(s_img_standby_mascot, LV_OPA_COVER, 0);
  }

  /* ── LOCKED view: padlock + unlock prompt ── */

  s_view_locked = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view_locked);
  lv_obj_set_size(s_view_locked, DISP_W, DISP_H);
  lv_obj_set_pos(s_view_locked, 0, 0);
  lv_obj_clear_flag(s_view_locked, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_locked, LV_SCROLLBAR_MODE_OFF);

  s_obj_locked_shackle = lv_obj_create(s_view_locked);
  lv_obj_remove_style_all(s_obj_locked_shackle);
  lv_obj_set_size(s_obj_locked_shackle, 42, 30);
  lv_obj_set_pos(s_obj_locked_shackle, (DISP_W - 42) / 2, 28);
  lv_obj_set_style_radius(s_obj_locked_shackle, 18, 0);
  lv_obj_set_style_border_width(s_obj_locked_shackle, 3, 0);
  lv_obj_set_style_border_color(s_obj_locked_shackle, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_obj_locked_shackle, LV_OPA_0, 0);

  s_obj_locked_body = lv_obj_create(s_view_locked);
  lv_obj_remove_style_all(s_obj_locked_body);
  lv_obj_set_size(s_obj_locked_body, 60, 52);
  lv_obj_set_pos(s_obj_locked_body, (DISP_W - 60) / 2, 52);
  lv_obj_set_style_radius(s_obj_locked_body, 12, 0);
  lv_obj_set_style_bg_color(s_obj_locked_body, lv_color_hex(0x163128), 0);
  lv_obj_set_style_bg_opa(s_obj_locked_body, LV_OPA_COVER, 0);
  lv_obj_set_style_border_width(s_obj_locked_body, 1, 0);
  lv_obj_set_style_border_color(s_obj_locked_body, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_border_opa(s_obj_locked_body, LV_OPA_70, 0);

  s_obj_locked_slot = lv_obj_create(s_obj_locked_body);
  lv_obj_remove_style_all(s_obj_locked_slot);
  lv_obj_set_size(s_obj_locked_slot, 10, 20);
  lv_obj_set_pos(s_obj_locked_slot, 25, 14);
  lv_obj_set_style_radius(s_obj_locked_slot, 5, 0);
  lv_obj_set_style_bg_color(s_obj_locked_slot, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_obj_locked_slot, LV_OPA_90, 0);

  s_lbl_locked_title = lv_label_create(s_view_locked);
  lv_obj_set_width(s_lbl_locked_title, body_w);
  lv_obj_set_style_text_color(s_lbl_locked_title, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_locked_title, font, 0);
  lv_obj_set_style_text_align(s_lbl_locked_title, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_locked_title, "设备已锁定");
  lv_obj_set_pos(s_lbl_locked_title, UI_SAFE_LEFT, 118);

  s_lbl_locked_hint = lv_label_create(s_view_locked);
  lv_obj_set_width(s_lbl_locked_hint, body_w);
  lv_obj_set_style_text_color(s_lbl_locked_hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_lbl_locked_hint, font, 0);
  lv_obj_set_style_text_align(s_lbl_locked_hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_lbl_locked_hint, "请按住说话键后说出密语");
  lv_obj_set_pos(s_lbl_locked_hint, UI_SAFE_LEFT, 140);

  /* ── ACTIVE view: status bar + text area ── */

  s_view_active = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view_active);
  lv_obj_set_size(s_view_active, DISP_W, DISP_H);
  lv_obj_set_pos(s_view_active, 0, 0);
  lv_obj_clear_flag(s_view_active, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_active, LV_SCROLLBAR_MODE_OFF);

  /* Status bar */
  /* Mode indicator (HOME/CLOUD) - leftmost */
  s_img_mode = lv_image_create(s_view_active);
  lv_obj_set_size(s_img_mode, UI_STATUS_ICON_SZ, UI_STATUS_ICON_SZ);
  lv_obj_set_pos(s_img_mode, UI_SAFE_LEFT, UI_SAFE_TOP + (status_h - UI_STATUS_ICON_SZ) / 2);
  lv_image_set_src(s_img_mode, s_cloud_mode ? &bb_img_mode_cloud : &bb_img_mode_home);

  /* Status icon - after mode indicator */
  s_img_status = lv_image_create(s_view_active);
  lv_image_set_src(s_img_status, &bb_img_ready);
  lv_obj_set_size(s_img_status, UI_STATUS_ICON_SZ, UI_STATUS_ICON_SZ);
  lv_obj_set_pos(s_img_status, UI_SAFE_LEFT + UI_STATUS_ICON_SZ + 4, UI_SAFE_TOP + (status_h - UI_STATUS_ICON_SZ) / 2);

  {
    const int wifi_w = 72;
    const int battery_enabled = (BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)) ? 1 : 0;
    const int battery_w = battery_enabled ? UI_BATTERY_W : 0;
    const int battery_gap = battery_enabled ? 4 : 0;
    const int status_text_x = UI_SAFE_LEFT + (UI_STATUS_ICON_SZ + 4) * 2 + 4;
    const int clock_w = 40;
    const int status_label_w = body_w - (UI_STATUS_ICON_SZ + 4) * 2 - 4 - wifi_w - battery_w - battery_gap - clock_w - 16;

    s_lbl_status = lv_label_create(s_view_active);
    lv_obj_set_width(s_lbl_status, status_label_w);
    lv_obj_set_style_text_color(s_lbl_status, lv_color_hex(UI_STATUS_FG), 0);
    lv_obj_set_style_text_font(s_lbl_status, font, 0);
    lv_label_set_long_mode(s_lbl_status, LV_LABEL_LONG_MODE_SCROLL_CIRCULAR);
    lv_obj_set_height(s_lbl_status, lh + 2);
    lv_label_set_text(s_lbl_status, BB_STATUS_BOOT);
    lv_obj_set_pos(s_lbl_status, status_text_x, UI_SAFE_TOP + (status_h - lh - 2) / 2);

    /* Clock in status bar (right side) */
    s_lbl_status_clock = lv_label_create(s_view_active);
    lv_obj_set_width(s_lbl_status_clock, clock_w);
    lv_obj_set_style_text_color(s_lbl_status_clock, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_status_clock, font, 0);
    lv_obj_set_style_text_align(s_lbl_status_clock, LV_TEXT_ALIGN_RIGHT, 0);
    lv_label_set_long_mode(s_lbl_status_clock, LV_LABEL_LONG_MODE_CLIP);
    lv_obj_set_height(s_lbl_status_clock, lh + 2);
    lv_label_set_text(s_lbl_status_clock, "--:--");
    lv_obj_set_pos(s_lbl_status_clock, UI_SAFE_LEFT + body_w - clock_w, UI_SAFE_TOP + (status_h - lh - 2) / 2);

    if (battery_enabled) {
      s_obj_status_battery = create_battery_widget(
          s_view_active,
          UI_SAFE_LEFT + body_w - clock_w - 6 - battery_w,
          UI_SAFE_TOP + (status_h - UI_BATTERY_H) / 2);
    }

    /* WiFi in status bar */
    s_obj_status_wifi = create_wifi_widget(s_view_active,
        UI_SAFE_LEFT + body_w - clock_w - 6 - battery_w - battery_gap - wifi_w,
        UI_SAFE_TOP + (status_h - 16) / 2,
        s_bar_status_wifi, &s_lbl_status_wifi_info, wifi_w);
  }

  /* Speaking area — shown only while TX is active */
  s_view_speaking = lv_obj_create(s_view_active);
  lv_obj_remove_style_all(s_view_speaking);
  lv_obj_set_size(s_view_speaking, body_w, content_h);
  lv_obj_set_pos(s_view_speaking, UI_SAFE_LEFT, content_y);
  lv_obj_clear_flag(s_view_speaking, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_speaking, LV_SCROLLBAR_MODE_OFF);

  {
    const int center_x = body_w / 2;
    const int badge_size = 42;
    const int halo_outer = UI_RECORD_HALO_BASE_PX + 14;
    const int halo_inner = UI_RECORD_HALO_BASE_PX;
    const int badge_x = center_x - badge_size / 2;
    const int badge_y = 10;
    const int halo_outer_x = center_x - halo_outer / 2;
    const int halo_inner_x = center_x - halo_inner / 2;

    s_obj_record_halo_outer = lv_obj_create(s_view_speaking);
    lv_obj_remove_style_all(s_obj_record_halo_outer);
    lv_obj_set_size(s_obj_record_halo_outer, halo_outer, halo_outer);
    lv_obj_set_pos(s_obj_record_halo_outer, halo_outer_x, badge_y - 6);
    lv_obj_set_style_radius(s_obj_record_halo_outer, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(s_obj_record_halo_outer, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_obj_record_halo_outer, LV_OPA_0, 0);

    s_obj_record_halo_inner = lv_obj_create(s_view_speaking);
    lv_obj_remove_style_all(s_obj_record_halo_inner);
    lv_obj_set_size(s_obj_record_halo_inner, halo_inner, halo_inner);
    lv_obj_set_pos(s_obj_record_halo_inner, halo_inner_x, badge_y + 1);
    lv_obj_set_style_radius(s_obj_record_halo_inner, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(s_obj_record_halo_inner, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_obj_record_halo_inner, LV_OPA_0, 0);

    s_obj_record_badge = lv_obj_create(s_view_speaking);
    lv_obj_remove_style_all(s_obj_record_badge);
    lv_obj_set_size(s_obj_record_badge, badge_size, badge_size);
    lv_obj_set_pos(s_obj_record_badge, badge_x, badge_y + 10);
    lv_obj_set_style_radius(s_obj_record_badge, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_border_width(s_obj_record_badge, 1, 0);
    lv_obj_set_style_border_color(s_obj_record_badge, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_bg_color(s_obj_record_badge, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_bg_opa(s_obj_record_badge, LV_OPA_20, 0);

    s_img_record_badge = lv_image_create(s_obj_record_badge);
    lv_image_set_src(s_img_record_badge, &bb_img_tx);
    lv_obj_center(s_img_record_badge);

    s_lbl_record_title = lv_label_create(s_view_speaking);
    lv_obj_set_width(s_lbl_record_title, body_w);
    lv_obj_set_style_text_color(s_lbl_record_title, lv_color_hex(UI_TEXT_MAIN), 0);
    lv_obj_set_style_text_font(s_lbl_record_title, font, 0);
    lv_obj_set_style_text_align(s_lbl_record_title, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_lbl_record_title, "正在聆听");
    lv_obj_set_pos(s_lbl_record_title, 0, 70);

    s_lbl_record_state = lv_label_create(s_view_speaking);
    lv_obj_set_width(s_lbl_record_state, body_w);
    lv_obj_set_style_text_color(s_lbl_record_state, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_text_font(s_lbl_record_state, font, 0);
    lv_obj_set_style_text_align(s_lbl_record_state, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_lbl_record_state, "请靠近麦克风说话");
    lv_obj_set_pos(s_lbl_record_state, 0, 88);

    s_lbl_record_hint = lv_label_create(s_view_speaking);
    lv_obj_set_width(s_lbl_record_hint, body_w);
    lv_obj_set_style_text_color(s_lbl_record_hint, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_record_hint, font, 0);
    lv_obj_set_style_text_align(s_lbl_record_hint, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_lbl_record_hint, "松开发送");
    lv_obj_set_pos(s_lbl_record_hint, 0, content_h - 16);

    s_obj_record_meter = lv_obj_create(s_view_speaking);
    lv_obj_remove_style_all(s_obj_record_meter);
    lv_obj_set_size(s_obj_record_meter,
                    UI_RECORD_BAR_COUNT * UI_RECORD_BAR_W + (UI_RECORD_BAR_COUNT - 1) * UI_RECORD_BAR_GAP,
                    UI_RECORD_BAR_MAX_H);
    lv_obj_set_pos(s_obj_record_meter,
                   (body_w - (UI_RECORD_BAR_COUNT * UI_RECORD_BAR_W + (UI_RECORD_BAR_COUNT - 1) * UI_RECORD_BAR_GAP)) / 2,
                   content_h - UI_RECORD_BAR_MAX_H - 26);
    lv_obj_clear_flag(s_obj_record_meter, LV_OBJ_FLAG_SCROLLABLE);

    for (int i = 0; i < UI_RECORD_BAR_COUNT; ++i) {
      s_obj_record_bar[i] = lv_obj_create(s_obj_record_meter);
      lv_obj_remove_style_all(s_obj_record_bar[i]);
      lv_obj_set_pos(s_obj_record_bar[i], i * (UI_RECORD_BAR_W + UI_RECORD_BAR_GAP),
                     UI_RECORD_BAR_MAX_H - UI_RECORD_BAR_MIN_H);
      lv_obj_set_style_radius(s_obj_record_bar[i], 3, 0);
      lv_obj_set_style_bg_color(s_obj_record_bar[i], lv_color_hex(UI_ME_ACCENT), 0);
      lv_obj_set_style_bg_opa(s_obj_record_bar[i], LV_OPA_70, 0);
      set_record_bar_height(s_obj_record_bar[i], UI_RECORD_BAR_MIN_H);
    }
  }

  /* Text area — full remaining space, pure text only */
  s_scroll_text = lv_obj_create(s_view_active);
  lv_obj_remove_style_all(s_scroll_text);
  lv_obj_set_size(s_scroll_text, body_w, content_h);
  lv_obj_set_pos(s_scroll_text, UI_SAFE_LEFT, content_y);
  lv_obj_add_flag(s_scroll_text, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scroll_dir(s_scroll_text, LV_DIR_VER);
  lv_obj_set_scrollbar_mode(s_scroll_text, LV_SCROLLBAR_MODE_OFF);

  s_lbl_text = lv_label_create(s_scroll_text);
  lv_obj_set_width(s_lbl_text, body_w - 4);
  lv_label_set_long_mode(s_lbl_text, LV_LABEL_LONG_MODE_WRAP);
  lv_obj_set_style_text_color(s_lbl_text, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_text, font, 0);
  lv_label_set_text(s_lbl_text, "");
  lv_obj_set_pos(s_lbl_text, 0, 0);

  /* Initial visibility */
  set_view_visible(s_view_standby, 1);
  set_view_visible(s_view_locked, 0);
  set_view_visible(s_view_active, 0);
  set_view_visible(s_view_speaking, 0);

  auto_scroll_ctx_attach(&s_auto_scroll_text, s_scroll_text);
  s_clock_timer = lv_timer_create(clock_timer_cb, 1000, NULL);
  s_auto_scroll_timer = lv_timer_create(auto_scroll_text_cb, UI_AUTO_SCROLL_PERIOD_MS, NULL);
  s_record_timer = lv_timer_create(record_timer_cb, UI_RECORD_UPDATE_MS, NULL);
  s_mascot_timer = lv_timer_create(mascot_timer_cb, UI_MASCOT_FRAME_MS, NULL);
  reset_recording_meter_visuals();
}

/* ── refresh_ui ── */

static void refresh_ui(void) {
  char status[sizeof(s_status)];
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  int turn_den = 0;
  int locked = 0;

  portENTER_CRITICAL(&s_state_lock);
  memcpy(status, s_status, sizeof(status));
  locked = s_locked;
  /* Auto-clear history after idle timeout to return to standby */
  if (BBCLAW_DISPLAY_STANDBY_TIMEOUT_MS > 0 && s_history_count > 0 && is_standby_status(s_status) &&
      s_last_active_ms > 0 && (bb_now_ms() - s_last_active_ms) >= BBCLAW_DISPLAY_STANDBY_TIMEOUT_MS) {
    s_history_count = 0;
    s_view_back = 0;
    s_scroll_you = 0;
    s_scroll_ai = 0;
  }
  turn_den = s_history_count;
  if (s_history_count <= 0) {
    you[0] = '\0';
    reply[0] = '\0';
  } else {
    int idx = s_history_count - 1 - s_view_back;
    if (idx < 0) idx = 0;
    memcpy(you, s_history[idx].you, sizeof(you));
    memcpy(reply, s_history[idx].reply, sizeof(reply));
    you[sizeof(you) - 1] = '\0';
    reply[sizeof(reply) - 1] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);

  if (!lvgl_port_lock(0)) return;

  const int64_t now_ms = bb_now_ms();
  char hm[8];
  format_clock(hm, sizeof(hm), now_ms);

  ui_view_mode_t mode = resolve_view_mode(status, turn_den, locked);
  const int recording = is_recording_status(status);

  set_view_visible(s_view_standby, mode == UI_VIEW_STANDBY);
  set_view_visible(s_view_locked, mode == UI_VIEW_LOCKED);
  set_view_visible(s_view_active, mode == UI_VIEW_ACTIVE);

  if (mode == UI_VIEW_STANDBY) {
    /* Update standby clock */
    lv_label_set_text(s_lbl_standby_clock, hm);
    s_record_view_visible = 0;
  } else if (mode == UI_VIEW_LOCKED) {
    if (strcmp(status, BB_STATUS_VERIFY_TX) == 0) {
      lv_label_set_text(s_lbl_locked_title, "正在聆听密语");
      lv_label_set_text(s_lbl_locked_hint, "松开按键后开始验证");
    } else if (strcmp(status, BB_STATUS_VERIFY) == 0) {
      lv_label_set_text(s_lbl_locked_title, "正在验证密语");
      lv_label_set_text(s_lbl_locked_hint, "请稍候");
    } else if (strcmp(status, BB_STATUS_VERIFY_ERR) == 0) {
      lv_label_set_text(s_lbl_locked_title, "解锁失败");
      lv_label_set_text(s_lbl_locked_hint, "请重新说出密语");
    } else {
      lv_label_set_text(s_lbl_locked_title, "设备已锁定");
      lv_label_set_text(s_lbl_locked_hint, "请按住说话键后说出密语");
    }
    s_record_view_visible = 0;
  } else {
    /* Status bar */
    const char* status_text = status;
    if (strcmp(status, BB_STATUS_TX) == 0) status_text = "LISTENING";
    else if (strcmp(status, BB_STATUS_RX) == 0 || strcmp(status, "TRANSCRIBING") == 0 || strcmp(status, "PROCESSING") == 0) status_text = "PROCESSING";
    lv_label_set_text(s_lbl_status, status_text[0] != '\0' ? status_text : BB_STATUS_READY);
    apply_status_icon(status);
    apply_wifi_bars(s_bar_status_wifi, s_lbl_status_wifi_info, status);
    apply_battery_widget();
    lv_label_set_text(s_lbl_status_clock, hm);

    set_view_visible(s_view_speaking, recording);
    set_view_visible(s_scroll_text, !recording);
    if (recording) {
      if (!s_record_view_visible) {
        reset_recording_meter_visuals();
      }
      s_record_view_visible = 1;
      refresh_recording_meter();
    } else {
      s_record_view_visible = 0;
    }

    /* Text area content */
    char buf[BBCLAW_DISPLAY_CHAT_LINE_LEN * 2 + 64];

    if (recording) {
      lv_label_set_text(s_lbl_record_title, "正在聆听");
      lv_label_set_text(s_lbl_record_hint, "松开发送");
    } else if (strcmp(status, BB_STATUS_RX) == 0 || strcmp(status, "TRANSCRIBING") == 0 || strcmp(status, "PROCESSING") == 0) {
      if (you[0] != '\0' && reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "我: %s\n答: %s", you, reply);
      } else if (you[0] != '\0') {
        snprintf(buf, sizeof(buf), "我: %s\n答: ...", you);
      } else {
        snprintf(buf, sizeof(buf), "处理中...");
      }
      lv_label_set_text(s_lbl_text, buf);
    } else if (strcmp(status, BB_STATUS_RESULT) == 0 || strcmp(status, BB_STATUS_SPEAK) == 0 ||
               strcmp(status, BB_STATUS_TASK) == 0 || strcmp(status, BB_STATUS_BUSY) == 0) {
      if (you[0] != '\0' || reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "我: %s\n答: %s",
                 you[0] != '\0' ? you : "--",
                 reply[0] != '\0' ? reply : "--");
      } else {
        snprintf(buf, sizeof(buf), "处理中...");
      }
      lv_label_set_text(s_lbl_text, buf);
    } else if (strcmp(status, BB_STATUS_WIFI_AP) == 0) {
      /* AP provisioning mode: show AP info from chat history */
      if (you[0] != '\0' || reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "%s\n%s", you, reply);
      } else {
        snprintf(buf, sizeof(buf), "AP 模式");
      }
      lv_label_set_text(s_lbl_text, buf);
    } else if (strncmp(status, BB_STATUS_BOOT, 4) == 0) {
      lv_label_set_text(s_lbl_text, "启动中...");
    } else if (strstr(status, BB_STATUS_WIFI) != NULL) {
      lv_label_set_text(s_lbl_text, "连接 WiFi...");
    } else if (strstr(status, BB_STATUS_ADAPTER) != NULL) {
      lv_label_set_text(s_lbl_text, "连接服务...");
    } else if (strcmp(status, BB_STATUS_PAIR) == 0) {
      /* Pairing: show registration code or detail from chat turn */
      if (reply[0] != '\0' && you[0] != '\0') {
        /* radio_app puts "Enter 6-digit code" in you, code in reply */
        size_t code_len = strlen(reply);
        if (code_len > 0 && code_len <= 12) {
          /* Space out digits for readability */
          char spaced[64];
          int pos = 0;
          for (size_t i = 0; i < code_len && pos < (int)sizeof(spaced) - 4; i++) {
            if (i > 0) spaced[pos++] = ' ';
            spaced[pos++] = reply[i];
          }
          spaced[pos] = '\0';
          snprintf(buf, sizeof(buf), "验证码\n%s", spaced);
        } else {
          snprintf(buf, sizeof(buf), "%s\n%s", you, reply);
        }
      } else if (you[0] != '\0') {
        snprintf(buf, sizeof(buf), "%s", you);
      } else {
        snprintf(buf, sizeof(buf), "等待配对...");
      }
      lv_label_set_text(s_lbl_text, buf);
    } else if (strstr(status, BB_STATUS_ERR) != NULL || strcmp(status, BB_STATUS_AUTH) == 0) {
      if (you[0] != '\0' || reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "%s\n%s",
                 you[0] != '\0' ? you : "",
                 reply[0] != '\0' ? reply : "");
        lv_label_set_text(s_lbl_text, buf);
      } else {
        lv_label_set_text(s_lbl_text, "错误");
      }
    } else if (turn_den > 0) {
      /* READY with history */
      snprintf(buf, sizeof(buf), "我: %s\n答: %s",
               you[0] != '\0' ? you : "--",
               reply[0] != '\0' ? reply : "--");
      lv_label_set_text(s_lbl_text, buf);
    } else {
      lv_label_set_text(s_lbl_text, "按住说话键开始对话");
    }

    /* Reset scroll on content change or view switch */
    if (!recording && (s_main_text_scroll_dirty || (int)mode != s_last_visible_mode)) {
      if (s_tts_playing && (int)mode == s_last_visible_mode) {
        /* TTS playing: don't reset to top, scroll to bottom instead */
        lv_obj_update_layout(s_scroll_text);
        int32_t max_y = lv_obj_get_scroll_bottom(s_scroll_text);
        if (max_y > 0) {
          lv_obj_scroll_to_y(s_scroll_text, max_y, LV_ANIM_OFF);
          s_auto_scroll_text.phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
          s_auto_scroll_text.wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
        }
      } else {
        auto_scroll_ctx_reset(&s_auto_scroll_text);
      }
      s_main_text_scroll_dirty = 0;
      s_main_text_scroll_to_bottom = 0;
    } else if (!recording && s_main_text_scroll_to_bottom) {
      s_main_text_scroll_to_bottom = 0;
      lv_obj_update_layout(s_scroll_text);
      int32_t max_y = lv_obj_get_scroll_bottom(s_scroll_text);
      if (max_y > 0) {
        lv_obj_scroll_to_y(s_scroll_text, max_y, LV_ANIM_OFF);
        s_auto_scroll_text.phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
        s_auto_scroll_text.wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
      }
    }
  }

  s_last_visible_mode = (int)mode;
  lvgl_port_unlock();
}

/* ── Public API ── */

esp_err_t bb_display_init(void) {
#if defined(BBCLAW_SIMULATOR)
  strncpy(s_status, BB_STATUS_BOOT, sizeof(s_status) - 1);
  s_status[sizeof(s_status) - 1] = '\0';
  s_history_count = 0;
  s_stream_turn_active = 0;
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_focus_ai = 1;
  s_auto_scroll_pause_until_ms = 0;
  s_locked = 0;
  s_main_text_scroll_dirty = 0;
  s_main_text_scroll_to_bottom = 0;
  s_last_visible_mode = -1;
  s_record_level_pct = 0;
  s_record_voiced = 0;
  s_record_level_updated_ms = 0;
  memset(s_record_bar_visual, 0, sizeof(s_record_bar_visual));
  s_record_anim_tick = 0;
  s_record_view_visible = 0;
  s_battery_available = 0;
  s_battery_percent = -1;
  s_battery_low = 0;
  s_battery_supported = 0;
  memset(&s_auto_scroll_text, 0, sizeof(s_auto_scroll_text));

  create_ui();
  s_ready = 1;
  refresh_ui();
  return ESP_OK;
#else
  backlight_on();
  strncpy(s_status, BB_STATUS_BOOT, sizeof(s_status) - 1);
  s_status[sizeof(s_status) - 1] = '\0';
  s_history_count = 0;
  s_stream_turn_active = 0;
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_focus_ai = 1;
  s_auto_scroll_pause_until_ms = 0;
  s_locked = 0;
  s_main_text_scroll_dirty = 0;
  s_main_text_scroll_to_bottom = 0;
  s_last_visible_mode = -1;
  s_record_level_pct = 0;
  s_record_voiced = 0;
  s_record_level_updated_ms = 0;
  memset(s_record_bar_visual, 0, sizeof(s_record_bar_visual));
  s_record_anim_tick = 0;
  s_record_view_visible = 0;
  s_battery_available = 0;
  s_battery_percent = -1;
  s_battery_low = 0;
  s_battery_supported = 0;
  memset(&s_auto_scroll_text, 0, sizeof(s_auto_scroll_text));

  ESP_RETURN_ON_ERROR(init_panel(), TAG, "panel init failed");

  lvgl_port_cfg_t lvgl_cfg = ESP_LVGL_PORT_INIT_CONFIG();
#if CONFIG_SOC_CPU_CORES_NUM > 1
  lvgl_cfg.task_affinity = 1;
#endif
  ESP_RETURN_ON_ERROR(lvgl_port_init(&lvgl_cfg), TAG, "lvgl_port_init failed");

  const lvgl_port_display_cfg_t disp_cfg = {
      .io_handle = s_panel_io,
      .panel_handle = s_panel,
      .control_handle = NULL,
      .buffer_size = (uint32_t)(DISP_W * 40),
      .double_buffer = true,
      .trans_size = 0,
      .hres = (uint32_t)DISP_W,
      .vres = (uint32_t)DISP_H,
      .monochrome = false,
      .rotation = {
          .swap_xy = (bool)DISP_SWAP_XY,
          .mirror_x = (bool)DISP_MIRROR_X,
          .mirror_y = (bool)DISP_MIRROR_Y,
      },
      .rounder_cb = NULL,
      .color_format = LV_COLOR_FORMAT_RGB565,
      .flags = {
          .buff_dma = 1,
          .buff_spiram = 0,
          .sw_rotate = 0,
          .swap_bytes = 1,
          .full_refresh = 0,
          .direct_mode = 0,
      },
  };

  lv_display_t* disp = lvgl_port_add_disp(&disp_cfg);
  if (disp == NULL) {
    ESP_LOGE(TAG, "lvgl_port_add_disp failed");
    return ESP_FAIL;
  }
  lv_display_set_flush_cb(disp, lvgl_flush_cb);

  if (!lvgl_port_lock(pdMS_TO_TICKS(2000))) return ESP_ERR_TIMEOUT;
  create_ui();
  lvgl_port_unlock();

  s_ready = 1;
  ESP_LOGI(TAG, "lvgl display ready %dx%d font=%s rgb=%s",
           DISP_W, DISP_H,
#ifdef BBCLAW_HAVE_CJK_FONT
           "bbclaw_cjk",
#else
           "default",
#endif
           DISP_RGB_ORDER_NAME);
  refresh_ui();
  return ESP_OK;
#endif
}

esp_err_t bb_display_show_status(const char* status_line) {
  if (status_line != NULL) {
    portENTER_CRITICAL(&s_state_lock);
    strncpy(s_status, status_line, sizeof(s_status) - 1);
    s_status[sizeof(s_status) - 1] = '\0';
    if (!is_standby_status(s_status)) s_last_active_ms = bb_now_ms();
    portEXIT_CRITICAL(&s_state_lock);
  }
  if (s_ready) refresh_ui();
  return ESP_OK;
}

esp_err_t bb_display_show_chat_turn(const char* user_said, const char* assistant_reply) {
  return bb_display_upsert_chat_turn(user_said, assistant_reply, 1);
}

esp_err_t bb_display_upsert_chat_turn(const char* user_said, const char* assistant_reply, int finalize) {
  const char* u = user_said != NULL ? user_said : "";
  const char* r = assistant_reply != NULL ? assistant_reply : "";
  if (u[0] == '\0' && r[0] == '\0') return ESP_OK;

  portENTER_CRITICAL(&s_state_lock);
  s_last_active_ms = bb_now_ms();
  if (s_stream_turn_active && s_history_count > 0) {
    strncpy(s_history[s_history_count - 1].you, u, sizeof(s_history[0].you) - 1);
    s_history[s_history_count - 1].you[sizeof(s_history[0].you) - 1] = '\0';
    strncpy(s_history[s_history_count - 1].reply, r, sizeof(s_history[0].reply) - 1);
    s_history[s_history_count - 1].reply[sizeof(s_history[0].reply) - 1] = '\0';
  } else if (s_history_count < BBCLAW_DISPLAY_CHAT_HISTORY) {
    strncpy(s_history[s_history_count].you, u, sizeof(s_history[0].you) - 1);
    s_history[s_history_count].you[sizeof(s_history[0].you) - 1] = '\0';
    strncpy(s_history[s_history_count].reply, r, sizeof(s_history[0].reply) - 1);
    s_history[s_history_count].reply[sizeof(s_history[0].reply) - 1] = '\0';
    s_history_count++;
  } else {
    memmove(&s_history[0], &s_history[1], sizeof(bb_chat_turn_t) * (BBCLAW_DISPLAY_CHAT_HISTORY - 1));
    strncpy(s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].you, u, sizeof(s_history[0].you) - 1);
    s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].you[sizeof(s_history[0].you) - 1] = '\0';
    strncpy(s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].reply, r, sizeof(s_history[0].reply) - 1);
    s_history[BBCLAW_DISPLAY_CHAT_HISTORY - 1].reply[sizeof(s_history[0].reply) - 1] = '\0';
  }
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_stream_turn_active = finalize ? 0 : 1;
  portEXIT_CRITICAL(&s_state_lock);

  if (s_ready) {
    if (finalize) {
      s_main_text_scroll_dirty = 1;
    } else {
      s_main_text_scroll_to_bottom = 1;
    }
    refresh_ui();
  }
  return ESP_OK;
}

esp_err_t bb_display_chat_prev_turn(void) {
  int scroll_reset = 0;
  portENTER_CRITICAL(&s_state_lock);
  if (s_history_count > 0 && s_view_back < s_history_count - 1) {
    s_view_back++;
    s_scroll_you = 0;
    s_scroll_ai = 0;
    scroll_reset = 1;
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) {
    if (scroll_reset) s_main_text_scroll_dirty = 1;
    refresh_ui();
  }
  return ESP_OK;
}

esp_err_t bb_display_chat_next_turn(void) {
  int scroll_reset = 0;
  portENTER_CRITICAL(&s_state_lock);
  if (s_view_back > 0) {
    s_view_back--;
    s_scroll_you = 0;
    s_scroll_ai = 0;
    scroll_reset = 1;
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) {
    if (scroll_reset) s_main_text_scroll_dirty = 1;
    refresh_ui();
  }
  return ESP_OK;
}

esp_err_t bb_display_chat_scroll_down(void) {
  const int step = line_px() * UI_MANUAL_SCROLL_STEP_LINES;
  const int64_t now_ms = bb_now_ms();

  portENTER_CRITICAL(&s_state_lock);
  if (s_focus_ai) s_scroll_ai++;
  else s_scroll_you++;
  portEXIT_CRITICAL(&s_state_lock);

  if (s_ready && lvgl_port_lock(pdMS_TO_TICKS(200))) {
    if (s_scroll_text != NULL) {
      lv_obj_update_layout(s_scroll_text);
      lv_obj_scroll_by_bounded(s_scroll_text, 0, step, LV_ANIM_OFF);
      auto_scroll_ctx_note_manual(&s_auto_scroll_text);
      s_auto_scroll_pause_until_ms = now_ms + UI_MANUAL_SCROLL_PAUSE_MS;
    }
    lvgl_port_unlock();
  }
  return ESP_OK;
}

esp_err_t bb_display_chat_scroll_up(void) {
  const int step = line_px() * UI_MANUAL_SCROLL_STEP_LINES;
  const int64_t now_ms = bb_now_ms();

  portENTER_CRITICAL(&s_state_lock);
  if (s_focus_ai) { if (s_scroll_ai > 0) s_scroll_ai--; }
  else { if (s_scroll_you > 0) s_scroll_you--; }
  portEXIT_CRITICAL(&s_state_lock);

  if (s_ready && lvgl_port_lock(pdMS_TO_TICKS(200))) {
    if (s_scroll_text != NULL) {
      lv_obj_update_layout(s_scroll_text);
      lv_obj_scroll_by_bounded(s_scroll_text, 0, -step, LV_ANIM_OFF);
      auto_scroll_ctx_note_manual(&s_auto_scroll_text);
      s_auto_scroll_pause_until_ms = now_ms + UI_MANUAL_SCROLL_PAUSE_MS;
    }
    lvgl_port_unlock();
  }
  return ESP_OK;
}

void bb_display_chat_focus_me(void) {
  portENTER_CRITICAL(&s_state_lock);
  s_focus_ai = 0;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

void bb_display_chat_focus_ai(void) {
  portENTER_CRITICAL(&s_state_lock);
  s_focus_ai = 1;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

void bb_display_set_cloud_mode(int is_cloud) {
  s_cloud_mode = is_cloud ? 1 : 0;
  if (s_ready && s_img_mode != NULL) {
    lv_image_set_src(s_img_mode, s_cloud_mode ? &bb_img_mode_cloud : &bb_img_mode_home);
  }
}

void bb_display_set_locked(int locked) {
  portENTER_CRITICAL(&s_state_lock);
  s_locked = locked ? 1 : 0;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

void bb_display_set_record_level(uint8_t level_pct, int voiced) {
  if (level_pct > 100U) {
    level_pct = 100U;
  }
  portENTER_CRITICAL(&s_state_lock);
  s_record_level_pct = level_pct;
  s_record_voiced = voiced ? 1 : 0;
  s_record_level_updated_ms = bb_now_ms();
  portEXIT_CRITICAL(&s_state_lock);
}

void bb_display_set_battery(int supported, int available, int percent, int low) {
  portENTER_CRITICAL(&s_state_lock);
  s_battery_supported = supported ? 1 : 0;
  s_battery_available = available ? 1 : 0;
  s_battery_percent = percent;
  s_battery_low = low ? 1 : 0;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

void bb_display_set_tts_playing(int playing) {
  s_tts_playing = playing ? 1 : 0;
}

void bb_display_set_tts_sentence(const char* sentence_text) {
  if (sentence_text == NULL || sentence_text[0] == '\0' || !s_ready) return;
  if (s_lbl_text == NULL || s_scroll_text == NULL) return;
  if (!lvgl_port_lock(0)) return;

  const char* full = lv_label_get_text(s_lbl_text);
  if (full == NULL) { lvgl_port_unlock(); return; }
  const char* pos = strstr(full, sentence_text);
  if (pos == NULL) { lvgl_port_unlock(); return; }

  /* lv_label_get_letter_pos takes a character index (not byte offset).
   * Count UTF-8 characters from start to the match position. */
  uint32_t char_idx = 0;
  for (const char* p = full; p < pos; char_idx++) {
    uint8_t c = (uint8_t)*p;
    if (c < 0x80) p += 1;
    else if (c < 0xE0) p += 2;
    else if (c < 0xF0) p += 3;
    else p += 4;
  }

  lv_obj_update_layout(s_scroll_text);
  lv_point_t lpos = {0};
  lv_label_get_letter_pos(s_lbl_text, char_idx, &lpos);

  int32_t target_y = lpos.y > 4 ? lpos.y - 4 : 0;
  int32_t max_y = lv_obj_get_scroll_bottom(s_scroll_text);
  if (target_y > max_y) target_y = max_y;
  lv_obj_scroll_to_y(s_scroll_text, target_y, LV_ANIM_ON);
  s_auto_scroll_text.phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
  s_auto_scroll_text.wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
  s_auto_scroll_pause_until_ms = bb_now_ms() + UI_MANUAL_SCROLL_PAUSE_MS;

  lvgl_port_unlock();
}
