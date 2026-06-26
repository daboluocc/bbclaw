/**
 * ST7789 + LVGL display — three-zone layout.
 *
 * STANDBY: BBClaw brand logo + animated clock + mascot.
 * LOCKED: padlock + unlock prompt.
 * ACTIVE: top status bar + full-screen scrollable text area.
 */
#include "bb_display.h"
#include "bb_page_standby.h"
#include "bb_page_locked.h"
#include "bb_chat_recording.h"
#include "bb_status.h"
#include "bb_ui_theme.h"

#if defined(BBCLAW_SIMULATOR)
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_lvgl_element_assets.h"
#include "bb_lvgl_assets.h"
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
#include <math.h>
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_lvgl_element_assets.h"
#include "bb_lvgl_assets.h"
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
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#endif

/* Dev diagnostic: time each LVGL refresh (REFR_START→REFR_READY) and log the
 * ones that hold the LVGL lock long enough to delay state-event delivery. A
 * heavy refresh is exactly what stalls the dispatch→render path (the drain
 * task / lv_async_call can only run between refreshes), so this pinpoints
 * which UI state is responsible for the ~240ms 跟手 lag. */
/* Off by default — flip to 1 locally when profiling render latency. Must stay
 * 0 in shipped builds: it ESP_LOGWs on every heavy refresh (and bb_bar mode
 * changes), which would spam real-device logs. NOT gated on
 * CONFIG_BBCLAW_DEVICE_MONITOR because that is intentionally =y in production
 * (ADR-015 CDC1 monitor ships on the PCB). */
#ifndef BBCLAW_LVGL_REFR_PROFILE
#define BBCLAW_LVGL_REFR_PROFILE 0
#endif
#define BBCLAW_LVGL_REFR_PROFILE_MIN_MS 25

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

/* Colors — design/UI_DESIGN_LANGUAGE.md tokens */
#define UI_SCR_BG        BB_UI_BG
#define UI_TEXT_MAIN     BB_UI_DOT_LIT
#define UI_TEXT_DIM      BB_UI_TEXT_DIM
#define UI_STATUS_FG     BB_UI_TEXT_DIM
#define UI_ME_ACCENT     BB_UI_ACCENT

/* Layout */
#define UI_SAFE_LEFT     10
#define UI_SAFE_RIGHT    12
#define UI_SAFE_TOP      8
#define UI_SAFE_BOTTOM   10
#define UI_GAP           2
#define UI_STATUS_ICON_SZ 16

/* Auto-scroll */
#define UI_AUTO_SCROLL_PERIOD_MS       96   /* timer interval: re-evaluate anim need, see #149 #155 */
#define UI_AUTO_SCROLL_ANIM_MS         200  /* lv_anim duration for scroll-to-bottom.
                                             * ADR-028 跟手:曾是 800ms——流式回复期间
                                             * 滚动动画几乎连续触发,动画每帧重绘整个
                                             * 滚动区,LVGL 任务长期占锁(真机日志
                                             * lvgl_port_lock timeout 集中在此窗口)。
                                             * 200ms 保留平滑感,重绘窗口缩到 1/4。 */
#define UI_AUTO_SCROLL_STEP_PX         1    /* threshold only (not a stepping value) */
#define UI_AUTO_SCROLL_TOP_HOLD_TICKS  12
#define UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS 14
#define UI_MANUAL_SCROLL_STEP_LINES    2
#define UI_MANUAL_SCROLL_PAUSE_MS      4000

/* WiFi bars */
#define UI_WIFI_BAR_COUNT  4
#define UI_WIFI_BAR_W      3
#define UI_WIFI_BAR_GAP    2
#define UI_WIFI_BAR_H_STEP 4

/* Battery widget — iOS-style icon (no percent text in topbar) */
#define UI_BATTERY_W        24  /* total container width (frame 22 + cap 2) */
#define UI_BATTERY_H        11  /* total container height */
#define UI_BATTERY_FRAME_W  22  /* outer rounded-rect width */
#define UI_BATTERY_FRAME_H  11  /* outer rounded-rect height */
#define UI_BATTERY_FILL_W   18  /* max fill width inside frame */
#define UI_BATTERY_FILL_H    7  /* fill height inside frame */
#define UI_BATTERY_CAP_W     2  /* positive terminal cap width */
#define UI_BATTERY_CAP_H     5  /* positive terminal cap height */

/* Large battery for LOCKED page */
#define UI_BATTERY_LG_W         60  /* total container width (frame 56 + cap 4) */
#define UI_BATTERY_LG_H         24  /* total container height */
#define UI_BATTERY_LG_FRAME_W   56  /* outer rounded-rect width */
#define UI_BATTERY_LG_FRAME_H   24  /* outer rounded-rect height */
#define UI_BATTERY_LG_FILL_W    50  /* max fill width inside frame */
#define UI_BATTERY_LG_FILL_H    18  /* fill height inside frame */
#define UI_BATTERY_LG_CAP_W      4  /* positive terminal cap width */
#define UI_BATTERY_LG_CAP_H     12  /* positive terminal cap height */

/* Bottom strip height — the old dot-matrix sweep band that lived here was
 * removed (UI calm-down pass); the height is kept only so the content-area
 * math below leaves the same quiet margin at the bottom of the screen. */
#define UI_BOTTOM_BAR_H    16
/* Status-bar activity-dot tick (also the legacy record-meter cadence).
 * Aligned to LV_DEF_REFR_PERIOD=16ms: 3×16. */
#define UI_BAR_UPDATE_MS   48

/* Recording speaking view */
#define UI_RECORD_UPDATE_MS       48  /* aligned to LV_DEF_REFR_PERIOD=16ms: 3×16, see #149 #155 */
/* Speaking-view VU — dot-matrix columns (design/UI_DESIGN_LANGUAGE.md):
 * 10 columns × 5 rows, lit bottom-up with level, peak dot teal while
 * voiced. Smoothing still runs in virtual px (MIN_H..MAX_H) and maps to
 * lit-row count at paint time — same idiom as bb_chat_recording.c. */
#define UI_RECORD_BAR_COUNT       10
#define UI_RECORD_DOT_ROWS        5
#define UI_RECORD_DOT             5
#define UI_RECORD_DOT_PITCH       8
#define UI_RECORD_BAR_MIN_H       6
#define UI_RECORD_BAR_MAX_H       38
#define UI_RECORD_METER_W         ((UI_RECORD_BAR_COUNT - 1) * UI_RECORD_DOT_PITCH + UI_RECORD_DOT)
#define UI_RECORD_METER_H         ((UI_RECORD_DOT_ROWS - 1) * UI_RECORD_DOT_PITCH + UI_RECORD_DOT)
#define UI_RECORD_HALO_BASE_PX    54
#define UI_RECORD_HALO_SPAN_PX    18
#define UI_RECORD_LEVEL_STALE_MS  280

/* Panel config */
#define DISP_X_GAP         BBCLAW_ST7789_X_GAP
#define DISP_Y_GAP         BBCLAW_ST7789_Y_GAP
#define DISP_PCLK_HZ       BBCLAW_ST7789_PCLK_HZ
#define DISP_SWAP_XY        BBCLAW_ST7789_SWAP_XY
#define DISP_MIRROR_X       BBCLAW_ST7789_MIRROR_X
#define DISP_MIRROR_Y       BBCLAW_ST7789_MIRROR_Y
/* DISP_SWAP_BYTES is no longer consumed in flush_cb (software swap removed).
 * Byte-swapping is now delegated to esp_lvgl_port via disp_cfg.flags.swap_bytes.
 * BBCLAW_ST7789_SWAP_BYTES on board_config.h controls whether the panel io layer
 * does a hardware swap; set to 0 to avoid double-swap. */
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
/* ADR-038: ASR-recognized passphrase text to overlay on the LOCKED page hint
 * ("听到「…」请重说"). Re-applied on every refresh while set, so an intervening
 * battery/status refresh during the post-reject hold doesn't wipe it. Cleared by
 * bb_display_show_status (any status change drops it). */
static char s_locked_heard[128];
static char s_bottom_session[64];
/* ADR-016: bottom-bar right side now shows the active model id/label so
 * users can see "what brain am I talking to" at a glance. */
static char s_bottom_model[40];
/* ADR-016 polish: friendly alias (logical session title from adapter) shown
 * on the bottom-bar left instead of the raw sid when non-empty. */
static char s_bottom_alias[24];
/* ADR-017: when set, bottom-bar left cell shows a reading-mode hint instead
 * of the session alias/sid. Toggled by bb_chat_transcript via
 * bb_display_set_reading_hint. */
static int s_reading_hint_on;

/* ADR-021-firmware-ui §1.2: butler cwd for bottom_bar "[B] <cwd>" left cell */
static char s_butler_cwd[48];

/* ADR-021-firmware-ui §1.3: mem stats for bottom_bar "mem: N+M" right cell.
 * -1 means unknown → renders as "mem: ?". */
static int s_mem_inbox   = -1;
static int s_mem_profile = -1;

/* ADR-021-firmware-ui §1.2: dispatch status state machine.
 * Tracks the current butler dispatch phase and auto-revert timer. */
typedef enum {
  DISPATCH_PHASE_NONE = 0,
  DISPATCH_PHASE_STARTED,
  DISPATCH_PHASE_DONE,
  DISPATCH_PHASE_ASYNC,
  DISPATCH_PHASE_ERROR,
} dispatch_phase_t;

typedef struct {
  dispatch_phase_t phase;
  char cwd[48];
  char task_id[64];
  int64_t elapsed_ms;
  /* non-NULL while a timed revert is pending */
  lv_timer_t* revert_timer;
} bb_dispatch_state_t;

static bb_dispatch_state_t s_dispatch = {0};

/* LVGL objects — locked moved to bb_page_locked.c */

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
static lv_obj_t* s_obj_status_battery_frame;
static lv_obj_t* s_obj_status_battery_fill;
static lv_obj_t* s_obj_status_battery_cap;
static lv_obj_t* s_obj_status_battery_charge_lbl; /* ⚡ overlay for charging state */
static lv_obj_t* s_lbl_status_battery_pct;        /* numeric "NN%" left of the icon */

/* Conversation activity state. The heavy full-width dot-matrix sweep strip that
 * used to live at the bottom of the ACTIVE view was removed (UI calm-down pass):
 * it repainted a 48-column canvas every 48ms — the dominant LVGL load during
 * voice — and cluttered the screen. The live conversation state is now shown by
 * a single small "breathing" dot in the top status bar (s_obj_listen_dot),
 * pulsed by s_bar_timer, plus a Chinese status word (聆听中…/识别中…/回复中…). */
typedef enum {
  BAR_IDLE = 0, /* READY/RESULT — no activity dot */
  BAR_LISTEN,   /* TX — dot tracks the live mic envelope (跟手) */
  BAR_PROCESS,  /* RX/TASK/BOOT/WIFI… — dot slow breathe (识别中) */
  BAR_SPEAK,    /* SPEAK — dot slow breathe (回复中) */
  BAR_ERROR,    /* ERR / NO WIFI / AUTH — dot red heartbeat */
} bottombar_mode_t;
static uint32_t s_bar_tick; /* frame counter — time base for the activity dot */
static int s_bottombar_mode;
static lv_timer_t* s_bar_timer;
static lv_obj_t* s_obj_listen_dot; /* top-bar "live activity" breathing dot */
static lv_obj_t* s_view_speaking;
static lv_obj_t* s_obj_record_halo_outer;
static lv_obj_t* s_obj_record_halo_inner;
static lv_obj_t* s_obj_record_badge;
static lv_obj_t* s_img_record_badge;
static lv_obj_t* s_lbl_record_title;
static lv_obj_t* s_lbl_record_state;
static lv_obj_t* s_lbl_record_hint;
static lv_obj_t* s_obj_record_meter;
/* Canvas replaces s_obj_record_dot[10][5] object matrix. */
static lv_color_t s_vu_canvas_buf[UI_RECORD_METER_W * UI_RECORD_METER_H];
static lv_draw_buf_t s_vu_draw_buf;
static lv_obj_t* s_obj_vu_canvas;
static lv_obj_t* s_scroll_text;
static lv_obj_t* s_lbl_text;

/* LVGL objects — standby moved to bb_page_standby.c */

/* Timers */
static lv_timer_t* s_clock_timer;
static lv_timer_t* s_auto_scroll_timer;
static lv_timer_t* s_record_timer;

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
static int s_battery_available;
static int s_battery_percent = -1;
static int s_battery_low;
static int s_battery_supported;
static int s_battery_charging;
static int s_cloud_mode;  /* 1 = cloud_saas, 0 = local_home */
static int s_chat_active; /* 1 when agent chat overlay is active */

static void refresh_ui(void);

/* ── Fonts ── */

static const lv_font_t* ui_font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

static int line_px(void) {
  return (int)lv_font_get_line_height(ui_font()) + 1;
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
  const int level = wifi_signal_level(status);
  lv_color_t on = lv_color_hex(UI_ME_ACCENT);
  lv_color_t off = lv_color_hex(UI_TEXT_DIM);
  for (int i = 0; i < UI_WIFI_BAR_COUNT; ++i) {
    if (bars[i] == NULL) continue;
    lv_obj_set_style_bg_color(bars[i], i < level ? on : off, 0);
    lv_obj_set_style_bg_opa(bars[i], i < level ? LV_OPA_COVER : LV_OPA_50, 0);
  }
  if (info_lbl != NULL) { /* SSID label retired, but keep the path for callers that still pass one */
    char wifi_info[64];
    format_wifi_info(wifi_info, sizeof(wifi_info));
    lv_label_set_text(info_lbl, wifi_info);
  }
}

static void apply_battery_widget(void) {
  if (s_obj_status_battery == NULL || s_obj_status_battery_fill == NULL) return;

  int supported = 0;
  int available = 0;
  int percent = -1;
  int low = 0;
  int charging = 0;
  portENTER_CRITICAL(&s_state_lock);
  supported = s_battery_supported;
  available = s_battery_available;
  percent = s_battery_percent;
  low = s_battery_low;
  charging = s_battery_charging;
  portEXIT_CRITICAL(&s_state_lock);

  if (!supported) {
    lv_obj_add_flag(s_obj_status_battery, LV_OBJ_FLAG_HIDDEN);
    if (s_lbl_status_battery_pct != NULL) lv_obj_add_flag(s_lbl_status_battery_pct, LV_OBJ_FLAG_HIDDEN);
    return;
  }

  lv_obj_clear_flag(s_obj_status_battery, LV_OBJ_FLAG_HIDDEN);
  if (!available || percent < 0) {
    /* No reading yet — show empty frame, hide fill + % */
    lv_obj_add_flag(s_obj_status_battery_fill, LV_OBJ_FLAG_HIDDEN);
    if (s_obj_status_battery_charge_lbl != NULL)
      lv_obj_add_flag(s_obj_status_battery_charge_lbl, LV_OBJ_FLAG_HIDDEN);
    if (s_lbl_status_battery_pct != NULL) lv_obj_add_flag(s_lbl_status_battery_pct, LV_OBJ_FLAG_HIDDEN);
    return;
  }

  if (percent > 100) percent = 100;
  if (percent < 0) percent = 0;

  /* Choose fill color by state: charging > low > normal */
  uint32_t fill_color;
  if (charging) {
    fill_color = BB_UI_OK;
  } else if (low) {
    fill_color = BB_UI_ERR;
  } else {
    fill_color = UI_TEXT_MAIN; /* cool white — standard battery look */
  }

  /* Fill width: charging shows full bar regardless of percent */
  int fill_w = charging ? UI_BATTERY_FILL_W : (percent * UI_BATTERY_FILL_W) / 100;
  if (fill_w < 1 && percent > 0) fill_w = 1; /* always show at least 1px when non-zero */

  lv_obj_clear_flag(s_obj_status_battery_fill, LV_OBJ_FLAG_HIDDEN);
  lv_obj_set_width(s_obj_status_battery_fill, fill_w);
  lv_obj_set_style_bg_color(s_obj_status_battery_fill, lv_color_hex(fill_color), 0);

  /* Frame border color matches fill for charging/low, dim otherwise */
  if (s_obj_status_battery_frame != NULL) {
    lv_obj_set_style_border_color(s_obj_status_battery_frame,
                                  lv_color_hex(charging ? BB_UI_OK : (low ? BB_UI_ERR : UI_STATUS_FG)), 0);
  }

  /* Charging lightning overlay */
  if (s_obj_status_battery_charge_lbl != NULL) {
    if (charging) {
      lv_obj_clear_flag(s_obj_status_battery_charge_lbl, LV_OBJ_FLAG_HIDDEN);
    } else {
      lv_obj_add_flag(s_obj_status_battery_charge_lbl, LV_OBJ_FLAG_HIDDEN);
    }
  }

  /* Numeric "NN%" left of the icon — dim normal, red low, green charging. */
  if (s_lbl_status_battery_pct != NULL) {
    char pct_buf[8];
    snprintf(pct_buf, sizeof(pct_buf), "%d%%", percent);
    lv_label_set_text(s_lbl_status_battery_pct, pct_buf);
    lv_obj_set_style_text_color(s_lbl_status_battery_pct,
                                lv_color_hex(charging ? BB_UI_OK : (low ? BB_UI_ERR : UI_TEXT_DIM)), 0);
    lv_obj_clear_flag(s_lbl_status_battery_pct, LV_OBJ_FLAG_HIDDEN);
  }
}

/* >=0 while chat mode drives the bar from agent state (see
 * bb_display_set_agent_bar_state); overrides the status-string mapping below
 * because chat mode intentionally stops updating the legacy s_status. */
static int s_agent_bar_mode = -1;

/* Map the live status string to a sweep mode (color + speed). The strip is
 * the active view's persistent ambient indicator across all states. */
static void apply_bottom_bar(const char* status, int recording) {
  if (s_chat_active && s_agent_bar_mode >= 0) {
    s_bottombar_mode = s_agent_bar_mode; /* chat agent state owns the bar */
    return;
  }
  int mode = BAR_IDLE;
  if (status != NULL && status[0] != '\0') {
    if (strstr(status, BB_STATUS_ERR) != NULL || strstr(status, "NO WIFI") != NULL ||
        strstr(status, BB_STATUS_AUTH) != NULL) {
      mode = BAR_ERROR;
    } else if (recording || strcmp(status, BB_STATUS_TX) == 0) {
      mode = BAR_LISTEN;
    } else if (strcmp(status, BB_STATUS_RX) == 0 || strcmp(status, "TRANSCRIBING") == 0 ||
               strcmp(status, "PROCESSING") == 0 || strcmp(status, BB_STATUS_TASK) == 0 ||
               strcmp(status, BB_STATUS_BUSY) == 0 || strncmp(status, BB_STATUS_BOOT, 4) == 0 ||
               strstr(status, BB_STATUS_WIFI) != NULL || strstr(status, BB_STATUS_ADAPTER) != NULL ||
               strstr(status, BB_STATUS_PAIR) != NULL) {
      mode = BAR_PROCESS;
    } else if (strcmp(status, BB_STATUS_SPEAK) == 0) {
      mode = BAR_SPEAK;
    }
  }
#if BBCLAW_LVGL_REFR_PROFILE
  if (mode != s_bottombar_mode) {
    ESP_LOGI("bb_bar", "mode %d->%d status='%s' rec=%d", s_bottombar_mode, mode, status ? status : "", recording);
  }
#endif
  s_bottombar_mode = mode;
}

static lv_opa_t clamp_opa(int v) { return (lv_opa_t)(v < 0 ? 0 : v > 255 ? 255 : v); }

/* Diameter of the top-bar activity dot (s_obj_listen_dot). */
#define UI_LISTEN_DOT_SZ 9

/* ── Top-bar "live activity" dot ──────────────────────────────────────────
 * Replaces the removed bottom dot-matrix strip. A single small dot in the
 * status bar carries the whole conversation state — far cheaper than the old
 * 48-column canvas repaint, and it leaves the transcript area uncluttered:
 *   LISTEN  → opacity tracks the live mic envelope (跟手 — immediate feedback)
 *   PROCESS → calm slow breathe   (识别中…)
 *   SPEAK   → calm slow breathe   (回复中…)
 *   ERROR   → red heartbeat double-pulse
 *   IDLE    → dot hidden, the static status icon shows in its place
 * Driven by s_bar_timer; early-returns (does no paint) while idle or hidden. */
static void bottombar_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_obj_listen_dot == NULL) return;
  if (s_view_active == NULL || lv_obj_has_flag(s_view_active, LV_OBJ_FLAG_HIDDEN)) return;

  /* The dot is a chat-mode indicator. In the legacy non-chat recording path the
   * full-screen meter owns s_img_status, so leave the icon alone there. */
  const int active = s_chat_active &&
                     (s_bottombar_mode == BAR_LISTEN || s_bottombar_mode == BAR_PROCESS ||
                      s_bottombar_mode == BAR_SPEAK  || s_bottombar_mode == BAR_ERROR);
  if (!active) {
    /* Idle: hide the dot, restore the static status icon, stop animating. */
    if (!lv_obj_has_flag(s_obj_listen_dot, LV_OBJ_FLAG_HIDDEN)) {
      lv_obj_add_flag(s_obj_listen_dot, LV_OBJ_FLAG_HIDDEN);
      if (s_img_status != NULL) lv_obj_clear_flag(s_img_status, LV_OBJ_FLAG_HIDDEN);
    }
    return;
  }
  /* Active: the dot owns the status-icon slot. */
  if (lv_obj_has_flag(s_obj_listen_dot, LV_OBJ_FLAG_HIDDEN)) {
    lv_obj_clear_flag(s_obj_listen_dot, LV_OBJ_FLAG_HIDDEN);
    if (s_img_status != NULL) lv_obj_add_flag(s_img_status, LV_OBJ_FLAG_HIDDEN);
  }

  s_bar_tick++;
  uint32_t color = BB_UI_ACCENT;
  lv_opa_t opa;

  if (s_bottombar_mode == BAR_LISTEN) {
    int level, voiced;
    int64_t updated;
    portENTER_CRITICAL(&s_state_lock);
    level   = s_record_level_pct;
    voiced  = s_record_voiced;
    updated = s_record_level_updated_ms;
    portEXIT_CRITICAL(&s_state_lock);
    /* Short stale window so the dot drops the instant the user stops talking
     * (跟手 decay); soft opacity floor keeps it visible between words. */
    if (updated == 0 || (bb_now_ms() - updated) > 200) { level = 0; voiced = 0; }
    if (level > 100) level = 100;
    opa   = clamp_opa(60 + level * 195 / 100);
    color = voiced ? BB_UI_ACCENT : UI_TEXT_DIM;
  } else if (s_bottombar_mode == BAR_ERROR) {
    /* red heartbeat — two close beats per cycle */
    float b = fmodf((float)s_bar_tick * 0.10f, 1.0f);
    float d0 = (b - 0.00f) / 0.07f, d1 = (b - 0.22f) / 0.07f;
    float e = expf(-d0 * d0), e1 = expf(-d1 * d1);
    if (e1 > e) e = e1;
    opa   = clamp_opa((int)((0.35f + e * 0.65f) * 255.0f));
    color = BB_UI_ERR;
  } else {
    /* PROCESS / SPEAK — calm indeterminate breathe */
    float o = 0.30f + (sinf((float)s_bar_tick * 0.16f) * 0.5f + 0.5f) * 0.70f;
    opa   = clamp_opa((int)(o * 255.0f));
    color = BB_UI_ACCENT;
  }

  lv_obj_set_style_bg_color(s_obj_listen_dot, lv_color_hex(color), 0);
  lv_obj_set_style_bg_opa(s_obj_listen_dot, opa, 0);
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

static int is_processing_status(const char* status) {
  if (status == NULL) return 0;
  if (strcmp(status, BB_STATUS_RX) == 0) return 1;
  if (strcmp(status, BB_STATUS_SPEAK) == 0) return 1;
  if (strcmp(status, BB_STATUS_TASK) == 0) return 1;
  if (strcmp(status, BB_STATUS_BUSY) == 0) return 1;
  if (strcmp(status, BB_STATUS_RESULT) == 0) return 1;
  if (strcmp(status, "TRANSCRIBING") == 0 || strcmp(status, "PROCESSING") == 0) return 1;
  if (strcmp(status, BB_STATUS_WIFI_ERR) == 0 || strncmp(status, BB_STATUS_WIFI_AP, 7) == 0) return 1;
  if (strcmp(status, BB_STATUS_NO_WIFI) == 0) return 1;
  if (strcmp(status, BB_STATUS_PAIR) == 0 || strcmp(status, BB_STATUS_AUTH) == 0) return 1;
  if (strncmp(status, "CLOUD", 5) == 0 || strncmp(status, "LINK", 4) == 0) return 1;
  return 0;
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

/* ── View mode ── */

static int should_show_locked_view(int locked, const char* status) {
  if (!locked) return 0;
  if (status == NULL || status[0] == '\0') return 1;
  if (strcmp(status, BB_STATUS_LOCKED) == 0 || strcmp(status, BB_STATUS_READY) == 0) return 1;
  if (strncmp(status, BB_STATUS_VERIFY, 6) == 0) return 1;
  return 0;
}

/* 待机判定：chat 没激活时，非活跃状态都显示 STANDBY（纯 BBClaw + 时钟）。
 * 不再看 turn_count — 对话历史在 chat overlay 里维护，STANDBY 页面始终干净。 */
static int is_standby_mode(const char* status, int turn_count) {
  (void)turn_count;
  if (is_recording_status(status) || is_processing_status(status)) return 0;
  if (strcmp(status, BB_STATUS_VERIFY_TX) == 0 ||
      strcmp(status, BB_STATUS_VERIFY) == 0 ||
      strcmp(status, BB_STATUS_VERIFY_ERR) == 0) return 0;
  return 1;
}

static ui_view_mode_t resolve_view_mode(const char* status, int locked, int turn_count) {
  if (should_show_locked_view(locked, status)) return UI_VIEW_LOCKED;
  /* chat overlay 激活时，即使没有对话历史也强制走 ACTIVE，
   * 让底层的顶栏/底栏显示出来（chat 用自己的 transcript 覆盖中间区）。 */
  if (s_chat_active) return UI_VIEW_ACTIVE;
  if (is_standby_mode(status, turn_count)) return UI_VIEW_STANDBY;
  return UI_VIEW_ACTIVE;
}

/* ── Clock ── */

static void format_clock(char* out, size_t out_size, int64_t now_ms) {
  if (out == NULL || out_size == 0) return;
  bb_wall_time_format_hm(out, out_size);
  if (((now_ms / 1000) & 1LL) != 0 && strlen(out) >= 3 && out[2] == ':') {
    out[2] = ' ';
  }
}

/* ── Auto-scroll ── */

/* Forward declaration — defined below, needed by auto_scroll_ctx_note_manual */
static void _scroll_anim_exec_cb(void* obj, int32_t val);

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
  /* Stop any running scroll animation so it doesn't override the user's position */
  lv_anim_delete(ctx->cont, _scroll_anim_exec_cb);
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

/* Map one column's smoothed virtual height to lit dots, bottom-up. Lit dots
 * are cool white; the peak dot is teal while voiced.
 * Paints directly into the VU canvas buffer via lv_canvas_set_px. */
static void set_record_column(int col, int height, int voiced) {
  if (s_obj_vu_canvas == NULL) return;
  if (height < UI_RECORD_BAR_MIN_H) {
    height = UI_RECORD_BAR_MIN_H;
  } else if (height > UI_RECORD_BAR_MAX_H) {
    height = UI_RECORD_BAR_MAX_H;
  }
  int rows_lit = 1 + ((height - UI_RECORD_BAR_MIN_H) * (UI_RECORD_DOT_ROWS - 1) +
                      (UI_RECORD_BAR_MAX_H - UI_RECORD_BAR_MIN_H) / 2) /
                         (UI_RECORD_BAR_MAX_H - UI_RECORD_BAR_MIN_H);
  for (int r = 0; r < UI_RECORD_DOT_ROWS; ++r) {
    int from_bottom = UI_RECORD_DOT_ROWS - r;
    uint32_t color;
    if (from_bottom > rows_lit) {
      color = BB_UI_DOT_GHOST;
    } else if (from_bottom == rows_lit && voiced) {
      color = UI_ME_ACCENT; /* peak dot */
    } else {
      color = UI_TEXT_MAIN;
    }
    lv_color_t c = lv_color_hex(color);
    int px = col * UI_RECORD_DOT_PITCH;
    int py = r * UI_RECORD_DOT_PITCH;
    for (int dy = 0; dy < UI_RECORD_DOT; dy++) {
      for (int dx = 0; dx < UI_RECORD_DOT; dx++) {
        lv_canvas_set_px(s_obj_vu_canvas, px + dx, py + dy, c, LV_OPA_COVER);
      }
    }
  }
}

static void reset_recording_meter_visuals(void) {
  s_record_anim_tick = 0;
  for (int i = 0; i < UI_RECORD_BAR_COUNT; ++i) {
    s_record_bar_visual[i] = UI_RECORD_BAR_MIN_H;
    set_record_column(i, UI_RECORD_BAR_MIN_H, 0);
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
  if (s_obj_vu_canvas != NULL) {
    lv_obj_invalidate(s_obj_vu_canvas);
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
    set_record_column(i, current_h, voiced);
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
  /* Single invalidate for the VU canvas — replaces per-dot set_style calls */
  if (s_obj_vu_canvas != NULL) {
    lv_obj_invalidate(s_obj_vu_canvas);
  }
}

/* anim exec callback: wraps lv_obj_scroll_to_y to match lv_anim_exec_xcb_t signature */
static void _scroll_anim_exec_cb(void* obj, int32_t val) {
  lv_obj_scroll_to_y((lv_obj_t*)obj, val, LV_ANIM_OFF);
}

/* ready callback: fires when the scroll animation completes */
static void _scroll_anim_ready_cb(lv_anim_t* a) {
  ui_auto_scroll_ctx_t* ctx = (ui_auto_scroll_ctx_t*)lv_anim_get_user_data(a);
  if (ctx == NULL) return;
  if (ctx->phase == UI_AUTO_SCROLL_RUNNING) {
    ctx->phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
    ctx->wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
  }
}

/* Start a smooth lv_anim scroll from current y to max_y. */
static void auto_scroll_start_anim(ui_auto_scroll_ctx_t* ctx, int32_t from_y, int32_t to_y) {
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, ctx->cont);
  lv_anim_set_exec_cb(&a, _scroll_anim_exec_cb);
  lv_anim_set_values(&a, from_y, to_y);
  lv_anim_set_duration(&a, UI_AUTO_SCROLL_ANIM_MS);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_completed_cb(&a, _scroll_anim_ready_cb);
  lv_anim_set_user_data(&a, ctx);
  lv_anim_start(&a);
}

static void auto_scroll_step_ctx(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL || ctx->cont == NULL || !scroll_cont_chain_visible(ctx->cont)) return;
  int32_t max_y = lv_obj_get_scroll_bottom(ctx->cont);
  if (max_y <= UI_AUTO_SCROLL_STEP_PX) {
    auto_scroll_ctx_reset(ctx);
    return;
  }
  int32_t y = lv_obj_get_scroll_y(ctx->cont);
  switch (ctx->phase) {
    case UI_AUTO_SCROLL_HOLD_TOP:
      if (y != 0) lv_obj_scroll_to_y(ctx->cont, 0, LV_ANIM_OFF);
      if (ctx->wait_ticks > 0) {
        ctx->wait_ticks--;
      } else {
        ctx->phase = UI_AUTO_SCROLL_RUNNING;
        /* Kick off the smooth animation toward the bottom */
        if (lv_anim_get(ctx->cont, _scroll_anim_exec_cb) == NULL) {
          auto_scroll_start_anim(ctx, y, max_y);
        }
      }
      break;
    case UI_AUTO_SCROLL_HOLD_BOTTOM:
      if (y < max_y) lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
      if (ctx->wait_ticks > 0) {
        ctx->wait_ticks--;
      } else if (s_tts_playing) {
        ctx->wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
      } else {
        /* TTS 结束，切换到 IDLE 状态，不再自动滚动 */
        ctx->phase = UI_AUTO_SCROLL_IDLE;
      }
      break;
    case UI_AUTO_SCROLL_IDLE:
      /* 停在底部，等待用户手动滚动或新内容到达后通过 auto_scroll_ctx_reset 重置 */
      if (y < max_y) lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
      break;
    case UI_AUTO_SCROLL_RUNNING:
    default:
      /* Each timer tick: delete any running anim and restart toward current max_y.
       * This handles new-content arrival (max_y grows) gracefully — the new anim
       * picks up from the current scroll position so there is no visible jump. */
      lv_anim_delete(ctx->cont, _scroll_anim_exec_cb);
      if (y >= max_y - UI_AUTO_SCROLL_STEP_PX) {
        lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
        ctx->phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
        ctx->wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
      } else {
        auto_scroll_start_anim(ctx, y, max_y);
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

/* Partial clock refresh — only updates clock-related labels instead of calling
 * refresh_ui() in full, eliminating the per-second CPU spike caused by a full
 * lv_scr_act() invalidation on every tick. See #149 Phase 2 / #155.
 *
 * All other UI state (battery, status text, etc.) is updated reactively via
 * the explicit refresh_ui() calls in the public API functions (bb_display_set_*),
 * so skipping them here causes no stale-state issues.
 *
 * s_clock_timer fires at 1000ms — this is semantic (whole-second tick) and is
 * intentionally NOT aligned to LV_DEF_REFR_PERIOD; phase drift is acceptable
 * because only a cheap lv_label_set_text is performed, not a full redraw. */
static void refresh_clock_only(void) {
  const int64_t now_ms = bb_now_ms();
  char hm[8];
  format_clock(hm, sizeof(hm), now_ms);

  int locked = 0;
  char status[sizeof(s_status)];
  int turn_den = 0;
  portENTER_CRITICAL(&s_state_lock);
  memcpy(status, s_status, sizeof(status));
  locked = s_locked;
  turn_den = s_history_count;
  portEXIT_CRITICAL(&s_state_lock);

  ui_view_mode_t mode = resolve_view_mode(status, locked, turn_den);

  if (!lvgl_port_lock(0)) return;
  if (mode == UI_VIEW_STANDBY) {
    bb_page_standby_refresh_clock(hm);
  } else if (mode == UI_VIEW_ACTIVE) {
    lv_label_set_text(s_lbl_status_clock, hm);
  }
  /* UI_VIEW_LOCKED does not display a clock widget — no action needed */
  lvgl_port_unlock();
}

static void clock_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_ready) refresh_clock_only();
}

/* ── View visibility ── */

static void set_view_visible(lv_obj_t* obj, int visible) {
  if (obj == NULL) return;
  if (visible) lv_obj_clear_flag(obj, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(obj, LV_OBJ_FLAG_HIDDEN);
}

/* ── WiFi bar widget creation helper ── */

/* Signal-bars-only WiFi glyph. The SSID text that used to sit left of the bars
 * was dropped (it crowded the battery off the right edge); the ascending bars
 * already say "WiFi + strength". info_w is just the bars' footprint. */
static lv_obj_t* create_wifi_widget(lv_obj_t* parent, int x, int y, lv_obj_t* bars[], lv_obj_t** info_lbl, int info_w) {
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

  if (info_lbl != NULL) *info_lbl = NULL; /* SSID label retired */
  return container;
}

static lv_obj_t* create_battery_widget(lv_obj_t* parent, int x, int y) {
  /* iOS-style battery icon: rounded-rect frame + fill bar + positive terminal cap.
   * No percent text — topbar space is tight. Charging state shown via ⚡ overlay. */
  lv_obj_t* container = lv_obj_create(parent);
  lv_obj_remove_style_all(container);
  lv_obj_set_size(container, UI_BATTERY_W, UI_BATTERY_H);
  lv_obj_set_pos(container, x, y);
  lv_obj_clear_flag(container, LV_OBJ_FLAG_SCROLLABLE);

  /* Fill bar — drawn first so frame border renders on top */
  s_obj_status_battery_fill = lv_obj_create(container);
  lv_obj_remove_style_all(s_obj_status_battery_fill);
  /* Position: 1px inset from frame border on all sides */
  lv_obj_set_size(s_obj_status_battery_fill, UI_BATTERY_FILL_W, UI_BATTERY_FILL_H);
  lv_obj_set_pos(s_obj_status_battery_fill, 2, 2);
  lv_obj_set_style_radius(s_obj_status_battery_fill, 1, 0);
  lv_obj_set_style_bg_color(s_obj_status_battery_fill, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_bg_opa(s_obj_status_battery_fill, LV_OPA_COVER, 0);

  /* Outer frame — rounded rect, no background fill, just border */
  s_obj_status_battery_frame = lv_obj_create(container);
  lv_obj_remove_style_all(s_obj_status_battery_frame);
  lv_obj_set_size(s_obj_status_battery_frame, UI_BATTERY_FRAME_W, UI_BATTERY_FRAME_H);
  lv_obj_set_pos(s_obj_status_battery_frame, 0, 0);
  lv_obj_set_style_radius(s_obj_status_battery_frame, 2, 0);
  lv_obj_set_style_border_width(s_obj_status_battery_frame, 1, 0);
  lv_obj_set_style_border_color(s_obj_status_battery_frame, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_border_opa(s_obj_status_battery_frame, LV_OPA_COVER, 0);
  lv_obj_set_style_bg_opa(s_obj_status_battery_frame, LV_OPA_0, 0);
  lv_obj_clear_flag(s_obj_status_battery_frame, LV_OBJ_FLAG_SCROLLABLE);

  /* Positive terminal cap — small rect on the right side, vertically centered */
  s_obj_status_battery_cap = lv_obj_create(container);
  lv_obj_remove_style_all(s_obj_status_battery_cap);
  lv_obj_set_size(s_obj_status_battery_cap, UI_BATTERY_CAP_W, UI_BATTERY_CAP_H);
  lv_obj_set_pos(s_obj_status_battery_cap,
                 UI_BATTERY_FRAME_W,
                 (UI_BATTERY_FRAME_H - UI_BATTERY_CAP_H) / 2);
  lv_obj_set_style_radius(s_obj_status_battery_cap, 1, 0);
  lv_obj_set_style_bg_color(s_obj_status_battery_cap, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_bg_opa(s_obj_status_battery_cap, LV_OPA_COVER, 0);

  /* Charging lightning label — centered over the frame, hidden by default */
  s_obj_status_battery_charge_lbl = lv_label_create(container);
  lv_obj_set_style_text_color(s_obj_status_battery_charge_lbl, lv_color_hex(BB_UI_BG), 0);
  lv_obj_set_style_text_font(s_obj_status_battery_charge_lbl, lv_font_get_default(), 0);
  lv_label_set_text(s_obj_status_battery_charge_lbl, LV_SYMBOL_CHARGE);
  lv_obj_set_pos(s_obj_status_battery_charge_lbl, 4, 0);
  lv_obj_add_flag(s_obj_status_battery_charge_lbl, LV_OBJ_FLAG_HIDDEN);

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

/* lvgl_flush_cb removed: esp_lvgl_port's internal flush callback (registered by
 * lvgl_port_add_disp) handles draw_bitmap and byte-swapping via flags.swap_bytes.
 * The application no longer overrides lv_display_set_flush_cb, so the port-owned
 * callback runs directly — eliminating the per-frame CPU swap scan that was here. */
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
  const int content_h = DISP_H - content_y - UI_SAFE_BOTTOM - UI_BOTTOM_BAR_H - UI_GAP;
  const int bottom_bar_y = DISP_H - UI_SAFE_BOTTOM - UI_BOTTOM_BAR_H;

  /* ── LOCKED view — delegated to bb_page_locked ── */
  bb_page_locked_create(scr);

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
  const int status_icon_x = UI_SAFE_LEFT + UI_STATUS_ICON_SZ + 4;
  const int status_icon_y = UI_SAFE_TOP + (status_h - UI_STATUS_ICON_SZ) / 2;
  lv_obj_set_pos(s_img_status, status_icon_x, status_icon_y);

  /* "Live activity" dot — occupies the status-icon slot while a conversation
   * is active (聆听/识别/回复/出错). The status-dot timer pulses it (mic-level
   * tracking while listening) and toggles it against the static icon. */
  s_obj_listen_dot = lv_obj_create(s_view_active);
  lv_obj_remove_style_all(s_obj_listen_dot);
  lv_obj_set_size(s_obj_listen_dot, UI_LISTEN_DOT_SZ, UI_LISTEN_DOT_SZ);
  lv_obj_set_pos(s_obj_listen_dot,
                 status_icon_x + (UI_STATUS_ICON_SZ - UI_LISTEN_DOT_SZ) / 2,
                 status_icon_y + (UI_STATUS_ICON_SZ - UI_LISTEN_DOT_SZ) / 2);
  lv_obj_set_style_radius(s_obj_listen_dot, LV_RADIUS_CIRCLE, 0);
  lv_obj_set_style_bg_color(s_obj_listen_dot, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_obj_listen_dot, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_obj_listen_dot, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_add_flag(s_obj_listen_dot, LV_OBJ_FLAG_HIDDEN);

  {
    /* Right side, right→left: clock · battery icon · "NN%" · WiFi bars.
     * SSID text dropped → WiFi is bars-only, leaving room for the % readout. */
    const int wifi_w = UI_WIFI_BAR_COUNT * UI_WIFI_BAR_W + (UI_WIFI_BAR_COUNT - 1) * UI_WIFI_BAR_GAP + 2;
    const int battery_enabled = (BBCLAW_POWER_ENABLE && (BBCLAW_POWER_ADC_GPIO >= 0)) ? 1 : 0;
    const int battery_w = battery_enabled ? UI_BATTERY_W : 0;
    const int batpct_w = battery_enabled ? 28 : 0; /* room for "100%" */
    const int batpct_gap = battery_enabled ? 3 : 0;
    const int wifi_gap = 6;
    const int clock_w = 40;
    const int status_text_x = UI_SAFE_LEFT + (UI_STATUS_ICON_SZ + 4) * 2 + 4;

    const int right = UI_SAFE_LEFT + body_w;
    const int clock_x = right - clock_w;
    const int batt_x = clock_x - 6 - battery_w;
    const int batpct_x = batt_x - batpct_gap - batpct_w; /* == batt_x when battery disabled */
    const int wifi_x = batpct_x - wifi_gap - wifi_w;
    const int status_label_w = wifi_x - status_text_x - 8;
    const int row_y = UI_SAFE_TOP + (status_h - lh - 2) / 2;

    s_lbl_status = lv_label_create(s_view_active);
    lv_obj_set_width(s_lbl_status, status_label_w);
    lv_obj_set_style_text_color(s_lbl_status, lv_color_hex(UI_STATUS_FG), 0);
    lv_obj_set_style_text_font(s_lbl_status, font, 0);
    lv_label_set_long_mode(s_lbl_status, LV_LABEL_LONG_MODE_SCROLL_CIRCULAR);
    lv_obj_set_height(s_lbl_status, lh + 2);
    lv_label_set_text(s_lbl_status, BB_STATUS_BOOT);
    lv_obj_set_pos(s_lbl_status, status_text_x, row_y);

    /* Clock in status bar (right side) */
    s_lbl_status_clock = lv_label_create(s_view_active);
    lv_obj_set_width(s_lbl_status_clock, clock_w);
    lv_obj_set_style_text_color(s_lbl_status_clock, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_status_clock, font, 0);
    lv_obj_set_style_text_align(s_lbl_status_clock, LV_TEXT_ALIGN_RIGHT, 0);
    lv_label_set_long_mode(s_lbl_status_clock, LV_LABEL_LONG_MODE_SCROLL_CIRCULAR);
    lv_obj_set_height(s_lbl_status_clock, lh + 2);
    lv_label_set_text(s_lbl_status_clock, "--:--");
    lv_obj_set_pos(s_lbl_status_clock, clock_x, row_y);

    if (battery_enabled) {
      s_obj_status_battery = create_battery_widget(
          s_view_active, batt_x, UI_SAFE_TOP + (status_h - UI_BATTERY_H) / 2);

      /* Numeric "NN%" just left of the icon */
      s_lbl_status_battery_pct = lv_label_create(s_view_active);
      lv_obj_set_width(s_lbl_status_battery_pct, batpct_w);
      lv_obj_set_style_text_color(s_lbl_status_battery_pct, lv_color_hex(UI_TEXT_DIM), 0);
      lv_obj_set_style_text_font(s_lbl_status_battery_pct, font, 0);
      lv_obj_set_style_text_align(s_lbl_status_battery_pct, LV_TEXT_ALIGN_RIGHT, 0);
      lv_obj_set_height(s_lbl_status_battery_pct, lh + 2);
      lv_label_set_text(s_lbl_status_battery_pct, "");
      lv_obj_set_pos(s_lbl_status_battery_pct, batpct_x, row_y);
    }

    /* WiFi in status bar — bars only */
    s_obj_status_wifi = create_wifi_widget(s_view_active, wifi_x,
        UI_SAFE_TOP + (status_h - 16) / 2,
        s_bar_status_wifi, &s_lbl_status_wifi_info, wifi_w);
  }

  /* Bottom bar removed (UI calm-down pass): the heavy full-width dot-matrix
   * sweep strip that used to sit here repainted a 48-column canvas every 48ms
   * and cluttered the screen. Conversation state is now shown by the top-bar
   * activity dot (s_obj_listen_dot) + a Chinese status word. The freed strip
   * at bottom_bar_y is left as quiet background. bb_page_locked keeps its own
   * footer. */
  (void)bottom_bar_y;

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
    lv_obj_set_size(s_obj_record_meter, UI_RECORD_METER_W, UI_RECORD_METER_H);
    lv_obj_set_pos(s_obj_record_meter, (body_w - UI_RECORD_METER_W) / 2,
                   content_h - UI_RECORD_METER_H - 26);
    lv_obj_clear_flag(s_obj_record_meter, LV_OBJ_FLAG_SCROLLABLE);

    /* VU canvas replaces the 10×5 dot object matrix */
    lv_draw_buf_init(&s_vu_draw_buf, UI_RECORD_METER_W, UI_RECORD_METER_H,
                     LV_COLOR_FORMAT_RGB565, LV_STRIDE_AUTO,
                     s_vu_canvas_buf, sizeof(s_vu_canvas_buf));
    s_obj_vu_canvas = lv_canvas_create(s_obj_record_meter);
    lv_canvas_set_draw_buf(s_obj_vu_canvas, &s_vu_draw_buf);
    lv_obj_set_pos(s_obj_vu_canvas, 0, 0);
    lv_obj_set_size(s_obj_vu_canvas, UI_RECORD_METER_W, UI_RECORD_METER_H);
    lv_canvas_fill_bg(s_obj_vu_canvas, lv_color_hex(0x000000), LV_OPA_COVER);
    for (int i = 0; i < UI_RECORD_BAR_COUNT; ++i) {
      set_record_column(i, UI_RECORD_BAR_MIN_H, 0);
    }
    lv_obj_invalidate(s_obj_vu_canvas);
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

  /* ── Standby view — delegated to bb_page_standby ── */
  bb_page_standby_create(scr);

  /* Initial visibility — STANDBY shown by default */
  bb_page_standby_set_visible(1);
  bb_page_locked_set_visible(0);
  set_view_visible(s_view_active, 0);
  set_view_visible(s_view_speaking, 0);

  auto_scroll_ctx_attach(&s_auto_scroll_text, s_scroll_text);
  /* 1000ms: semantic whole-second tick — not aligned to LV_DEF_REFR_PERIOD;
   * phase drift is acceptable because clock_timer_cb only does a cheap
   * lv_label_set_text (no full redraw). See #155. */
  s_clock_timer = lv_timer_create(clock_timer_cb, 1000, NULL);
  /* UI_AUTO_SCROLL_PERIOD_MS=96ms = 6×16 — aligned to LV_DEF_REFR_PERIOD=16ms */
  s_auto_scroll_timer = lv_timer_create(auto_scroll_text_cb, UI_AUTO_SCROLL_PERIOD_MS, NULL);
  /* UI_RECORD_UPDATE_MS=48ms = 3×16 — aligned to LV_DEF_REFR_PERIOD=16ms */
  s_record_timer = lv_timer_create(record_timer_cb, UI_RECORD_UPDATE_MS, NULL);
  /* UI_BAR_UPDATE_MS=48ms = 3×16 — aligned to LV_DEF_REFR_PERIOD=16ms */
  s_bar_timer = lv_timer_create(bottombar_timer_cb, UI_BAR_UPDATE_MS, NULL);
  reset_recording_meter_visuals();
}

/* ── refresh_ui ── */

static void refresh_ui(void) {
  char status[sizeof(s_status)];
  char heard[sizeof(s_locked_heard)];
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  int turn_den = 0;
  int locked = 0;

  portENTER_CRITICAL(&s_state_lock);
  memcpy(status, s_status, sizeof(status));
  memcpy(heard, s_locked_heard, sizeof(heard));
  locked = s_locked;
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

  ui_view_mode_t mode = resolve_view_mode(status, locked, turn_den);
  const int recording = is_recording_status(status);

  bb_page_standby_set_visible(mode == UI_VIEW_STANDBY);
  bb_page_locked_set_visible(mode == UI_VIEW_LOCKED);
  set_view_visible(s_view_active, mode == UI_VIEW_ACTIVE);

  if (mode == UI_VIEW_STANDBY) {
    bb_page_standby_refresh_clock(hm);
    {
      int bat_supported, bat_available, bat_percent, bat_low, bat_charging;
      portENTER_CRITICAL(&s_state_lock);
      bat_supported = s_battery_supported;
      bat_available = s_battery_available;
      bat_percent   = s_battery_percent;
      bat_low       = s_battery_low;
      bat_charging  = s_battery_charging;
      portEXIT_CRITICAL(&s_state_lock);
      bb_page_standby_update_battery(bat_supported, bat_available, bat_percent, bat_low, bat_charging);
    }
    s_record_view_visible = 0;
  } else if (mode == UI_VIEW_LOCKED) {
    bb_page_locked_update_status(status);
    bb_page_locked_show_heard(heard); /* ADR-038: overlay "听到「…」" after a failed unlock */
    {
      int bat_supported, bat_available, bat_percent, bat_low, bat_charging;
      portENTER_CRITICAL(&s_state_lock);
      bat_supported = s_battery_supported;
      bat_available = s_battery_available;
      bat_percent   = s_battery_percent;
      bat_low       = s_battery_low;
      bat_charging  = s_battery_charging;
      portEXIT_CRITICAL(&s_state_lock);
      bb_page_locked_update_battery(bat_supported, bat_available, bat_percent, bat_low, bat_charging);
    }
    s_record_view_visible = 0;
  } else {
    /* ACTIVE view (chat) — full layout with top status bar */
    const char* status_text = status;
    if (strcmp(status, BB_STATUS_TX) == 0) status_text = "聆听中…";
    else if (strcmp(status, BB_STATUS_RX) == 0 || strcmp(status, "TRANSCRIBING") == 0 || strcmp(status, "PROCESSING") == 0) status_text = "识别中…";
    else if (strcmp(status, BB_STATUS_SPEAK) == 0) status_text = "回复中…";

    /* In chat mode the legacy status string is intentionally stale; the agent
     * bar state is the authority for the conversation word. Keep it in sync
     * with the top-bar activity dot (s_obj_listen_dot). */
    if (s_chat_active && s_agent_bar_mode >= 0) {
      switch (s_agent_bar_mode) {
        case BAR_LISTEN:  status_text = "聆听中…"; break;
        case BAR_PROCESS: status_text = "识别中…"; break;
        case BAR_SPEAK:   status_text = "回复中…"; break;
        case BAR_ERROR:   status_text = "出错";    break;
        case BAR_IDLE:
        default:          status_text = "就绪";    break;
      }
    }

    /* ADR-021-firmware-ui §1.2: dispatch phase overlay — highest priority.
     * error > async > done > started > 常态 */
    {
      char dispatch_text[80] = {0};
      dispatch_phase_t dp = s_dispatch.phase;
      if (dp == DISPATCH_PHASE_ERROR) {
        snprintf(dispatch_text, sizeof(dispatch_text), "派发失败 ❌");
      } else if (dp == DISPATCH_PHASE_ASYNC) {
        if (s_dispatch.task_id[0] != '\0') {
          snprintf(dispatch_text, sizeof(dispatch_text), "已转异步 #%.12s", s_dispatch.task_id);
        } else {
          snprintf(dispatch_text, sizeof(dispatch_text), "已转异步");
        }
      } else if (dp == DISPATCH_PHASE_DONE) {
        long long elapsed_s = (long long)(s_dispatch.elapsed_ms / 1000LL);
        snprintf(dispatch_text, sizeof(dispatch_text), "worker 完成 ✅ (%llus)", elapsed_s);
      } else if (dp == DISPATCH_PHASE_STARTED) {
        if (s_dispatch.cwd[0] != '\0') {
          snprintf(dispatch_text, sizeof(dispatch_text), "派发中: %s…", s_dispatch.cwd);
        } else {
          snprintf(dispatch_text, sizeof(dispatch_text), "派发中…");
        }
      }
      if (dispatch_text[0] != '\0') {
        lv_label_set_text(s_lbl_status, dispatch_text);
      } else {
        lv_label_set_text(s_lbl_status, status_text[0] != '\0' ? status_text : BB_STATUS_READY);
      }
    }
    apply_status_icon(status);
    apply_wifi_bars(s_bar_status_wifi, s_lbl_status_wifi_info, status);
    apply_battery_widget();
    lv_label_set_text(s_lbl_status_clock, hm);
    apply_bottom_bar(status, recording);

    /* Legacy full-screen recording meter ("正在聆听 / 请靠近麦克风说话" + halo +
     * 10-col VU) is retired in chat mode — the top-bar activity dot is the sole
     * indicator there. Without the !s_chat_active gate, a stray BB_STATUS_TX
     * leaking in while the chat overlay is up would flash the old meter over the
     * transcript (the "之前的动画效果" bug). It still serves the legacy non-chat
     * radio path. */
    set_view_visible(s_view_speaking, recording && !s_chat_active);
    /* chat 激活时：底层对话文本区隐藏，中间留给 overlay 的 transcript */
    set_view_visible(s_scroll_text, !recording && !s_chat_active);

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

#if BBCLAW_LVGL_REFR_PROFILE
static int64_t s_refr_t0_us;
static lv_area_t s_inv_union;   /* bounding box of all areas invalidated this cycle */
static int s_inv_valid;
static void inv_area_cb(lv_event_t* e) {
  const lv_area_t* a = (const lv_area_t*)lv_event_get_param(e);
  if (!a) return;
  if (!s_inv_valid) {
    s_inv_union = *a;
    s_inv_valid = 1;
  } else {
    if (a->x1 < s_inv_union.x1) s_inv_union.x1 = a->x1;
    if (a->y1 < s_inv_union.y1) s_inv_union.y1 = a->y1;
    if (a->x2 > s_inv_union.x2) s_inv_union.x2 = a->x2;
    if (a->y2 > s_inv_union.y2) s_inv_union.y2 = a->y2;
  }
}
static void refr_start_cb(lv_event_t* e) {
  (void)e;
  s_refr_t0_us = esp_timer_get_time();
}
static void refr_ready_cb(lv_event_t* e) {
  (void)e;
  int64_t dt_ms = (esp_timer_get_time() - s_refr_t0_us) / 1000;
  lv_area_t u = s_inv_union;
  int valid = s_inv_valid;
  s_inv_valid = 0;
  if (dt_ms >= BBCLAW_LVGL_REFR_PROFILE_MIN_MS && valid) {
    ESP_LOGW("lvgl_refr", "heavy refresh %lldms bbox=[%d,%d→%d,%d] %dx%d — delays state→render",
             (long long)dt_ms, (int)u.x1, (int)u.y1, (int)u.x2, (int)u.y2, (int)lv_area_get_width(&u),
             (int)lv_area_get_height(&u));
  }
}
#endif

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
  s_battery_charging = 0;
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
  s_battery_charging = 0;
  memset(&s_auto_scroll_text, 0, sizeof(s_auto_scroll_text));

  ESP_RETURN_ON_ERROR(init_panel(), TAG, "panel init failed");

  lvgl_port_cfg_t lvgl_cfg = ESP_LVGL_PORT_INIT_CONFIG();
#if CONFIG_SOC_CPU_CORES_NUM > 1
  lvgl_cfg.task_affinity = 1;
#endif
  /* 默认优先级 4 低于语音管线（capture 7 / stream 5 / tts 5 / ws 5）——LVGL 持锁
   * 渲染时被抢占，锁挂着几百毫秒，等锁的 PTT dispatch(200ms)/agent_chat_enter(500ms)
   * 超时丢事件 → "不跟手"。提到 6：高于 stream/tts/ws(5)，低于 capture(7) 保音频不卡。
   * LVGL 每帧渲染后 sleep（LV_DEF_REFR_PERIOD=16），不是常驻 hog，不会饿死 ws。见 #149。 */
  lvgl_cfg.task_priority = 6;
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
#if BBCLAW_LVGL_REFR_PROFILE
  lv_display_add_event_cb(disp, refr_start_cb, LV_EVENT_REFR_START, NULL);
  lv_display_add_event_cb(disp, refr_ready_cb, LV_EVENT_REFR_READY, NULL);
  lv_display_add_event_cb(disp, inv_area_cb, LV_EVENT_INVALIDATE_AREA, NULL);
  ESP_LOGI(TAG, "lvgl refr profiler on (threshold %dms)", BBCLAW_LVGL_REFR_PROFILE_MIN_MS);
#endif
  /* NOTE: do NOT override lv_display_set_flush_cb here.
   * esp_lvgl_port registers its own flush callback (lvgl_port_flush_callback)
   * which handles esp_lcd_panel_draw_bitmap and, when flags.swap_bytes is set,
   * calls lv_draw_sw_rgb565_swap internally.  Overriding it would bypass that
   * logic and force the application to duplicate it (the old approach). */

  if (!lvgl_port_lock(2000)) return ESP_ERR_TIMEOUT;
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
    s_locked_heard[0] = '\0'; /* ADR-038: a status change drops the stale "听到" overlay */
    portEXIT_CRITICAL(&s_state_lock);
  }
  if (s_ready) refresh_ui();
  return ESP_OK;
}

esp_err_t bb_display_show_heard(const char* heard) {
  /* ADR-038: set the ASR-recognized passphrase text the LOCKED page overlays on
   * its hint. Call right AFTER bb_display_show_status(BB_STATUS_VERIFY_ERR) — that
   * call clears any previous heard, this one sets the new one; refresh re-applies
   * it. Empty/NULL clears it. */
  portENTER_CRITICAL(&s_state_lock);
  if (heard != NULL) {
    strncpy(s_locked_heard, heard, sizeof(s_locked_heard) - 1);
    s_locked_heard[sizeof(s_locked_heard) - 1] = '\0';
  } else {
    s_locked_heard[0] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);
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

  if (s_ready && lvgl_port_lock(200)) {
    if (s_scroll_text != NULL) {
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

  if (s_ready && lvgl_port_lock(200)) {
    if (s_scroll_text != NULL) {
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
  /* Forward to chat recording overlay when chat is active */
  if (s_chat_active) {
    bb_chat_recording_set_level(level_pct, voiced);
  }
}

void bb_display_set_battery(int supported, int available, int percent, int low, int charging) {
  portENTER_CRITICAL(&s_state_lock);
  s_battery_supported = supported ? 1 : 0;
  s_battery_available = available ? 1 : 0;
  s_battery_percent = percent;
  s_battery_low = low ? 1 : 0;
  s_battery_charging = charging ? 1 : 0;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

void bb_display_set_session_id(const char* session_id) {
  portENTER_CRITICAL(&s_state_lock);
  if (session_id == NULL) {
    s_bottom_session[0] = '\0';
  } else {
    strncpy(s_bottom_session, session_id, sizeof(s_bottom_session) - 1);
    s_bottom_session[sizeof(s_bottom_session) - 1] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

/* ADR-016 polish: bottom-bar left-cell alias (logical session title).
 * Non-empty takes priority over the raw sid; empty clears the override so
 * the next refresh falls back to the sid tail. */
void bb_display_set_session_alias(const char* alias) {
  portENTER_CRITICAL(&s_state_lock);
  if (alias == NULL) {
    s_bottom_alias[0] = '\0';
  } else {
    strncpy(s_bottom_alias, alias, sizeof(s_bottom_alias) - 1);
    s_bottom_alias[sizeof(s_bottom_alias) - 1] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

/* ADR-016: persisted active model id/label for display. The display only
 * keeps a string copy; Settings UI is the source of truth and pushes it
 * here via bb_ui_settings_ → bb_ui_agent_chat → bb_display chain. */
void bb_display_set_active_model(const char* model_label) {
  portENTER_CRITICAL(&s_state_lock);
  if (model_label == NULL) {
    s_bottom_model[0] = '\0';
  } else {
    strncpy(s_bottom_model, model_label, sizeof(s_bottom_model) - 1);
    s_bottom_model[sizeof(s_bottom_model) - 1] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

static int s_chat_active_stub;
void bb_display_set_chat_active(int active) {
  (void)s_chat_active_stub;
  s_chat_active = active ? 1 : 0;
  if (!s_chat_active) {
    s_agent_bar_mode = -1; /* hand the bar back to the status-string mapping */
  }
  if (s_ready) refresh_ui();
}

void bb_display_set_agent_bar_state(bb_bar_state_t state) {
  int mode;
  switch (state) {
    case BB_BAR_STATE_LISTENING: mode = BAR_LISTEN; break;
    case BB_BAR_STATE_BUSY:      mode = BAR_PROCESS; break;
    case BB_BAR_STATE_SPEAKING:  mode = BAR_SPEAK; break;
    case BB_BAR_STATE_ERROR:     mode = BAR_ERROR; break;
    case BB_BAR_STATE_IDLE:
    default:                     mode = BAR_IDLE; break;
  }
  s_agent_bar_mode = mode;
  s_bottombar_mode = mode; /* activity dot picks the new mode on its next tick */
#if BBCLAW_LVGL_REFR_PROFILE
  ESP_LOGI("bb_bar", "agent bar state=%d -> mode=%d (chat_active=%d)", (int)state, mode, s_chat_active);
#endif
  /* Refresh now so the top-bar status word (聆听中…/识别中…/回复中…/出错/就绪)
   * flips immediately with the state instead of waiting for the next unrelated
   * display update — chat mode otherwise rarely triggers a refresh. */
  if (s_ready) refresh_ui();
}

void bb_display_set_tts_playing(int playing) {
  s_tts_playing = playing ? 1 : 0;
}

void bb_display_set_reading_hint(int on) {
  int next = on ? 1 : 0;
  portENTER_CRITICAL(&s_state_lock);
  int changed = (s_reading_hint_on != next);
  s_reading_hint_on = next;
  portEXIT_CRITICAL(&s_state_lock);
  if (changed && s_ready) refresh_ui();
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
  /* ADR-028 跟手:逐句跟读改瞬时跳转。每句都触发一次滚动,LV_ANIM_ON 意味着
   * 每句拖一条动画(动画期间整滚动区逐帧重绘),TTS 播放窗口正是按键最需要
   * 响应的窗口。瞬时跳转 = 单次重绘。 */
  lv_obj_scroll_to_y(s_scroll_text, target_y, LV_ANIM_OFF);
  s_auto_scroll_text.phase = UI_AUTO_SCROLL_HOLD_BOTTOM;
  s_auto_scroll_text.wait_ticks = UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS;
  s_auto_scroll_pause_until_ms = bb_now_ms() + UI_MANUAL_SCROLL_PAUSE_MS;

  lvgl_port_unlock();
}

/* ── ADR-021-firmware-ui §1.2 / §1.3: dispatch status + footer helpers ── */

/* Revert timer callback: clears the dispatch overlay and re-renders status */
static void dispatch_revert_timer_cb(lv_timer_t* timer) {
  (void)timer;
  s_dispatch.phase = DISPATCH_PHASE_NONE;
  s_dispatch.revert_timer = NULL;
  if (s_ready) refresh_ui();
}

void bb_display_set_butler_cwd(const char* cwd) {
  portENTER_CRITICAL(&s_state_lock);
  if (cwd == NULL) {
    s_butler_cwd[0] = '\0';
  } else {
    strncpy(s_butler_cwd, cwd, sizeof(s_butler_cwd) - 1);
    s_butler_cwd[sizeof(s_butler_cwd) - 1] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}

void bb_display_set_dispatch_status(const char* phase, const char* cwd,
                                    const char* task_id, int64_t elapsed_ms) {
  if (phase == NULL || phase[0] == '\0') return;

  /* Cancel any pending revert timer before updating state */
  if (!lvgl_port_lock(50)) return;
  if (s_dispatch.revert_timer != NULL) {
    lv_timer_del(s_dispatch.revert_timer);
    s_dispatch.revert_timer = NULL;
  }
  lvgl_port_unlock();

  portENTER_CRITICAL(&s_state_lock);
  if (strcmp(phase, "started") == 0) {
    s_dispatch.phase = DISPATCH_PHASE_STARTED;
  } else if (strcmp(phase, "done") == 0) {
    s_dispatch.phase = DISPATCH_PHASE_DONE;
  } else if (strcmp(phase, "async") == 0) {
    s_dispatch.phase = DISPATCH_PHASE_ASYNC;
  } else if (strcmp(phase, "error") == 0) {
    s_dispatch.phase = DISPATCH_PHASE_ERROR;
  } else {
    portEXIT_CRITICAL(&s_state_lock);
    return;
  }
  if (cwd != NULL) {
    strncpy(s_dispatch.cwd, cwd, sizeof(s_dispatch.cwd) - 1);
    s_dispatch.cwd[sizeof(s_dispatch.cwd) - 1] = '\0';
  } else {
    s_dispatch.cwd[0] = '\0';
  }
  if (task_id != NULL) {
    strncpy(s_dispatch.task_id, task_id, sizeof(s_dispatch.task_id) - 1);
    s_dispatch.task_id[sizeof(s_dispatch.task_id) - 1] = '\0';
  } else {
    s_dispatch.task_id[0] = '\0';
  }
  s_dispatch.elapsed_ms = elapsed_ms;
  portEXIT_CRITICAL(&s_state_lock);

  if (s_ready) refresh_ui();

  /* done/async/error: schedule auto-revert after 4 seconds
   * 4000ms: semantic display timeout — not aligned to LV_DEF_REFR_PERIOD;
   * this is a one-shot UX timer, phase drift is irrelevant. See #155. */
  if (s_dispatch.phase != DISPATCH_PHASE_STARTED) {
    if (lvgl_port_lock(50)) {
      s_dispatch.revert_timer = lv_timer_create(dispatch_revert_timer_cb, 4000, NULL);
      if (s_dispatch.revert_timer != NULL) {
        lv_timer_set_repeat_count(s_dispatch.revert_timer, 1);
      }
      lvgl_port_unlock();
    }
  }
}

void bb_display_set_mem_stats(int inbox, int profile) {
  portENTER_CRITICAL(&s_state_lock);
  s_mem_inbox   = inbox;
  s_mem_profile = profile;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) refresh_ui();
}
