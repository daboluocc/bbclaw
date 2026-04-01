/**
 * ST7789 + LVGL（esp_lvgl_port），实现 bb_display.h 与旧自绘 bb_display_bitmap.c 相同 API。
 */
#include "bb_display.h"

#if defined(BBCLAW_SIMULATOR)
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
#else
#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_lvgl_element_assets.h"
#include "bb_lvgl_assets.h"
#include "bb_time.h"
#include "bb_wifi.h"
#include "driver/gpio.h"
#include "driver/spi_master.h"
#include "esp_check.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_lcd_panel_vendor.h"
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

#define SPEAK_BAR_COUNT 7

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

/* 横屏小面板：深色底 + 双色条区分角色（RGB565 友好） */
#define UI_SCR_BG 0x0a0e0c
#define UI_ME_BG 0x141c18
#define UI_AI_BG 0x121820
#define UI_ME_ACCENT 0x2ec4a0
#define UI_AI_ACCENT 0x4a9fd8
#define UI_TEXT_MAIN 0xd8ebe4
#define UI_TEXT_DIM 0x7a9a8c
#define UI_STATUS_FG 0x8fbcac
#define UI_PAD_OUT 3
#define UI_SAFE_LEFT 10
#define UI_SAFE_RIGHT 12
#define UI_SAFE_TOP 8
#define UI_SAFE_BOTTOM 10
#define UI_GAP 2
#define UI_STATUS_ICON_SZ 16
#define UI_FOOTER_LOGO_SZ 14
/** 靠边装饰缩小；主体给文本 */
#define UI_DECOR_SCALE_HERO 72
#define UI_DECOR_SCALE_AVATAR 96
#define UI_DECOR_SCALE_FOOTER 40
/** 正文不要用 lv_obj transform 缩放：LVGL9 对 transform 的 label 会分配整段 layer（WRAP 长文时巨量 memset），
 *  单次 lv_timer_handler 过久会饿死 IDLE 触发 task_wdt，进而表现像 UI/PTT「坏死」。
 *  需要更大字号请换更大 lv_font（如重跑 lv_font_conv），而不是 scale transform。 */
#define UI_NOTIFY_FOOTER_ROW 26
#define UI_AUTO_SCROLL_PERIOD_MS 96
#define UI_AUTO_SCROLL_STEP_PX 1
#define UI_AUTO_SCROLL_TOP_HOLD_TICKS 12
#define UI_AUTO_SCROLL_BOTTOM_HOLD_TICKS 14
#define UI_MANUAL_SCROLL_STEP_LINES 2
#define UI_MANUAL_SCROLL_PAUSE_MS 4000
#define UI_NOTIFICATION_MIN_MS 3200
#define UI_NOTIFICATION_DONE_GRACE_MS 1200
#define DISP_X_GAP BBCLAW_ST7789_X_GAP
#define DISP_Y_GAP BBCLAW_ST7789_Y_GAP
#define DISP_PCLK_HZ BBCLAW_ST7789_PCLK_HZ
#define DISP_SWAP_XY BBCLAW_ST7789_SWAP_XY
#define DISP_MIRROR_X BBCLAW_ST7789_MIRROR_X
#define DISP_MIRROR_Y BBCLAW_ST7789_MIRROR_Y
#define DISP_SWAP_BYTES BBCLAW_ST7789_SWAP_BYTES
#define DISP_INVERT_COLOR BBCLAW_ST7789_INVERT_COLOR
#if BBCLAW_ST7789_RGB_ORDER_BGR
#define DISP_RGB_ORDER LCD_RGB_ELEMENT_ORDER_BGR
#define DISP_RGB_ORDER_NAME "BGR"
#else
#define DISP_RGB_ORDER LCD_RGB_ELEMENT_ORDER_RGB
#define DISP_RGB_ORDER_NAME "RGB"
#endif
#define UI_DECOR_SCALE_MEDIUM 160
#define UI_DECOR_SCALE_LARGE 128
#define UI_DECOR_SCALE_SMALL 48
#define UI_GRID_LINE 0x2a4a5c
/* Figma notification / speaking */
#define UI_NOTIFY_BG 0x050a10
#define UI_NOTIFY_BORDER 0x15f5d6
#define UI_NOTIFY_TEXT_WHITE 0xffffff
#define UI_NOTIFY_TEXT_GRAY 0x8899aa
#define UI_NOTIFY_STATUS_GREEN 0x50fa7b
#define UI_NOTIFY_RADIUS 20
#define UI_WIFI_BAR_COUNT 4
#define UI_WIFI_BAR_W 3
#define UI_WIFI_BAR_GAP 2
#define UI_WIFI_BAR_H_STEP 4
#define UI_WIFI_INFO_W 108

typedef struct {
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
} bb_chat_turn_t;

typedef enum {
  SPEAK_ANIM_IDLE = 0,
  SPEAK_ANIM_TX,
  SPEAK_ANIM_RX,
  SPEAK_ANIM_SPEAK,
} speak_anim_mode_t;

typedef enum {
  UI_VIEW_STANDBY = 0,
  UI_VIEW_NOTIFICATION,
  UI_VIEW_SPEAKING,
} ui_view_mode_t;

typedef enum {
  UI_AUTO_SCROLL_HOLD_TOP = 0,
  UI_AUTO_SCROLL_RUNNING,
  UI_AUTO_SCROLL_HOLD_BOTTOM,
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

static bb_chat_turn_t s_history[BBCLAW_DISPLAY_CHAT_HISTORY];
static int s_history_count;
static int s_stream_turn_active;
static int s_view_back;
static int s_scroll_you;
static int s_scroll_ai;
static int s_focus_ai;
static char s_status[32];
static int64_t s_notification_until_ms;

static lv_obj_t* s_bg_decor;
static lv_obj_t* s_img_status;
static lv_obj_t* s_img_battery_frame;
static lv_obj_t* s_img_footer_logo;
static lv_obj_t* s_lbl_status;
static lv_obj_t* s_view_standby;
static lv_obj_t* s_img_standby_hero;
static lv_obj_t* s_img_standby_brand_claw;
static lv_obj_t* s_img_standby_brand_openclaw;
static lv_obj_t* s_lbl_standby_brand_join;
static lv_obj_t* s_lbl_standby_clock;
static lv_obj_t* s_obj_standby_wifi;
static lv_obj_t* s_lbl_standby_wifi_info;
static lv_obj_t* s_bar_standby_wifi[UI_WIFI_BAR_COUNT];
static lv_obj_t* s_scroll_standby_body;
static lv_obj_t* s_lbl_standby_body;
static lv_obj_t* s_lbl_standby_meta;
static lv_obj_t* s_view_notification;
static lv_obj_t* s_obj_notification_status_dot;
static lv_obj_t* s_lbl_notification_task;
static lv_obj_t* s_lbl_notification_new;
static lv_obj_t* s_obj_notification_divider;
static lv_obj_t* s_img_notification_avatar;
static lv_obj_t* s_lbl_notification_title;
static lv_obj_t* s_lbl_notification_body;
static lv_obj_t* s_lbl_notification_open;
static lv_obj_t* s_lbl_notification_dismiss;
static lv_obj_t* s_scroll_notification_text;
static lv_obj_t* s_view_speaking;
static lv_obj_t* s_img_speaking_hero;
static lv_obj_t* s_lbl_speaking_title;
static lv_obj_t* s_lbl_speaking_body;
static lv_obj_t* s_lbl_speaking_hint;
static lv_obj_t* s_scroll_speaking_text;
static lv_obj_t* s_lbl_ptt_indicator;
static lv_obj_t* s_bar_speaking[SPEAK_BAR_COUNT];
static lv_obj_t* s_lbl_footer;
static lv_obj_t* s_lbl_footer_wifi;
static lv_obj_t* s_lbl_footer_state;
static lv_obj_t* s_lbl_turn_info;
static lv_timer_t* s_clock_timer;
static lv_timer_t* s_auto_scroll_timer;
static lv_timer_t* s_speaking_anim_timer;
static int s_speaking_bar_base_x;
static int s_speaking_bar_base_y;
static int s_standby_clock_base_y;
static speak_anim_mode_t s_speaking_anim_mode;
static uint32_t s_speaking_anim_step;
static ui_auto_scroll_ctx_t s_auto_scroll_standby;
static ui_auto_scroll_ctx_t s_auto_scroll_notification;
static ui_auto_scroll_ctx_t s_auto_scroll_speaking;
static int64_t s_auto_scroll_pause_until_ms;
static int s_notification_scroll_complete;

static int s_ready;
/** bb_display_show_chat_turn / 轮次切换时置位，刷新后回到文本顶部再自动滚动 */
static int s_main_text_scroll_dirty;
static int s_last_visible_mode = -1;

static void refresh_ui(void);
static int is_speaking_status(const char* status);
static int is_notification_status(const char* status);

static lv_point_precise_t s_grid_pts[6][2];

static void standby_clock_anim_y_cb(void* obj, int32_t v) {
  lv_obj_set_y((lv_obj_t*)obj, (lv_coord_t)v);
}

static void add_screen_grid_decor(lv_obj_t* scr) {
  int n = 0;

  s_bg_decor = lv_obj_create(scr);
  lv_obj_remove_style_all(s_bg_decor);
  lv_obj_set_size(s_bg_decor, DISP_W, DISP_H);
  lv_obj_set_pos(s_bg_decor, 0, 0);
  lv_obj_clear_flag(s_bg_decor, LV_OBJ_FLAG_SCROLLABLE | LV_OBJ_FLAG_CLICKABLE);

  s_grid_pts[n][0].x = 6;
  s_grid_pts[n][0].y = 36;
  s_grid_pts[n][1].x = DISP_W - 6;
  s_grid_pts[n][1].y = 72;
  n++;
  s_grid_pts[n][0].x = 20;
  s_grid_pts[n][0].y = 96;
  s_grid_pts[n][1].x = DISP_W - 40;
  s_grid_pts[n][1].y = 130;
  n++;
  s_grid_pts[n][0].x = DISP_W * 2 / 3;
  s_grid_pts[n][0].y = 8;
  s_grid_pts[n][1].x = DISP_W - 12;
  s_grid_pts[n][1].y = 56;
  n++;
  s_grid_pts[n][0].x = 12;
  s_grid_pts[n][0].y = DISP_H - 40;
  s_grid_pts[n][1].x = DISP_W / 3;
  s_grid_pts[n][1].y = DISP_H - 16;
  n++;

  for (int i = 0; i < n; ++i) {
    lv_obj_t* ln = lv_line_create(s_bg_decor);
    lv_line_set_points(ln, s_grid_pts[i], 2);
    lv_obj_set_style_line_color(ln, lv_color_hex(UI_GRID_LINE), 0);
    lv_obj_set_style_line_width(ln, 1, 0);
    lv_obj_set_style_line_opa(ln, LV_OPA_30, 0);
  }

  lv_obj_move_to_index(s_bg_decor, 0);
}

static void clock_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_ready) {
    refresh_ui();
  }
}

static void notification_scroll_mark_complete(int64_t now_ms) {
  s_notification_scroll_complete = 1;
  if (s_notification_until_ms < now_ms + UI_NOTIFICATION_DONE_GRACE_MS) {
    s_notification_until_ms = now_ms + UI_NOTIFICATION_DONE_GRACE_MS;
  }
}

static void format_standby_clock(char* out, size_t out_size, int64_t now_ms) {
  if (out == NULL || out_size == 0) {
    return;
  }
  bb_wall_time_format_hm(out, out_size);
  if (((now_ms / 1000) & 1LL) != 0 && strlen(out) >= 3 && out[2] == ':') {
    out[2] = ' ';
  }
}

static ui_view_mode_t resolve_view_mode(const char* status, int turn_den, int64_t notification_until_ms, int64_t now_ms) {
  if (is_speaking_status(status)) {
    return UI_VIEW_SPEAKING;
  }
  if ((is_notification_status(status) || turn_den > 0) &&
      (now_ms < notification_until_ms || !s_notification_scroll_complete)) {
    return UI_VIEW_NOTIFICATION;
  }
  return UI_VIEW_STANDBY;
}

static lv_obj_t* active_scroll_cont_for_mode(ui_view_mode_t mode) {
  switch (mode) {
    case UI_VIEW_NOTIFICATION:
      return s_scroll_notification_text;
    case UI_VIEW_SPEAKING:
      return s_scroll_speaking_text;
    case UI_VIEW_STANDBY:
    default:
      return s_scroll_standby_body;
  }
}

static ui_auto_scroll_ctx_t* auto_scroll_ctx_for_mode(ui_view_mode_t mode) {
  switch (mode) {
    case UI_VIEW_NOTIFICATION:
      return &s_auto_scroll_notification;
    case UI_VIEW_SPEAKING:
      return &s_auto_scroll_speaking;
    case UI_VIEW_STANDBY:
    default:
      return &s_auto_scroll_standby;
  }
}

static void scroll_cont_reset_top(lv_obj_t* cont) {
  if (cont != NULL) {
    lv_obj_scroll_to_y(cont, 0, LV_ANIM_OFF);
  }
}

static void auto_scroll_ctx_attach(ui_auto_scroll_ctx_t* ctx, lv_obj_t* cont) {
  if (ctx == NULL) {
    return;
  }
  ctx->cont = cont;
  ctx->phase = UI_AUTO_SCROLL_HOLD_TOP;
  ctx->wait_ticks = UI_AUTO_SCROLL_TOP_HOLD_TICKS;
}

static void auto_scroll_ctx_reset(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL) {
    return;
  }
  ctx->phase = UI_AUTO_SCROLL_HOLD_TOP;
  ctx->wait_ticks = UI_AUTO_SCROLL_TOP_HOLD_TICKS;
  scroll_cont_reset_top(ctx->cont);
}

static void auto_scroll_ctx_note_manual(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL || ctx->cont == NULL) {
    return;
  }
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
    if (lv_obj_has_flag(p, LV_OBJ_FLAG_HIDDEN)) {
      return 0;
    }
  }
  return 1;
}

static void auto_scroll_step_ctx(ui_auto_scroll_ctx_t* ctx) {
  if (ctx == NULL || ctx->cont == NULL || !scroll_cont_chain_visible(ctx->cont)) {
    return;
  }
  lv_obj_update_layout(ctx->cont);
  int32_t max_y = lv_obj_get_scroll_bottom(ctx->cont);
  if (max_y <= UI_AUTO_SCROLL_STEP_PX) {
    if (ctx == &s_auto_scroll_notification) {
      notification_scroll_mark_complete(bb_now_ms());
    }
    auto_scroll_ctx_reset(ctx);
    return;
  }
  int32_t y = lv_obj_get_scroll_y(ctx->cont);
  switch (ctx->phase) {
    case UI_AUTO_SCROLL_HOLD_TOP:
      if (y != 0) {
        lv_obj_scroll_to_y(ctx->cont, 0, LV_ANIM_OFF);
      }
      if (ctx->wait_ticks > 0) {
        ctx->wait_ticks--;
      } else {
        ctx->phase = UI_AUTO_SCROLL_RUNNING;
      }
      break;
    case UI_AUTO_SCROLL_HOLD_BOTTOM:
      if (y < max_y) {
        lv_obj_scroll_to_y(ctx->cont, max_y, LV_ANIM_OFF);
      }
      if (ctx->wait_ticks > 0) {
        ctx->wait_ticks--;
      } else {
        if (ctx == &s_auto_scroll_notification) {
          notification_scroll_mark_complete(bb_now_ms());
        }
        auto_scroll_ctx_reset(ctx);
      }
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
  if (!s_ready) {
    return;
  }
  if (bb_now_ms() < s_auto_scroll_pause_until_ms) {
    return;
  }
  auto_scroll_step_ctx(&s_auto_scroll_standby);
  auto_scroll_step_ctx(&s_auto_scroll_notification);
  auto_scroll_step_ctx(&s_auto_scroll_speaking);
}

static void relayout_notification_text(void) {
  if (s_scroll_notification_text == NULL || s_lbl_notification_title == NULL || s_lbl_notification_body == NULL) {
    return;
  }
  lv_coord_t sw = lv_obj_get_width(s_scroll_notification_text);
  if (sw < 24) {
    return;
  }
  const lv_coord_t w = sw - 8;
  lv_obj_set_width(s_lbl_notification_title, w);
  lv_obj_set_width(s_lbl_notification_body, w);
  lv_label_set_long_mode(s_lbl_notification_title, LV_LABEL_LONG_MODE_WRAP);
  lv_label_set_long_mode(s_lbl_notification_body, LV_LABEL_LONG_MODE_WRAP);
  lv_obj_set_pos(s_lbl_notification_title, 0, 0);
  lv_obj_update_layout(s_lbl_notification_title);
  lv_coord_t th = lv_obj_get_height(s_lbl_notification_title);
  lv_obj_set_pos(s_lbl_notification_body, 0, th + 6);
  lv_obj_update_layout(s_lbl_notification_body);
  lv_obj_update_layout(s_scroll_notification_text);
}

static void relayout_speaking_text(void) {
  if (s_scroll_speaking_text == NULL || s_lbl_speaking_title == NULL || s_lbl_speaking_body == NULL) {
    return;
  }
  lv_coord_t sw = lv_obj_get_width(s_scroll_speaking_text);
  if (sw < 24) {
    return;
  }
  const lv_coord_t w = sw - 8;
  lv_obj_set_width(s_lbl_speaking_title, w);
  lv_obj_set_width(s_lbl_speaking_body, w);
  lv_label_set_long_mode(s_lbl_speaking_title, LV_LABEL_LONG_MODE_WRAP);
  lv_label_set_long_mode(s_lbl_speaking_body, LV_LABEL_LONG_MODE_WRAP);
  lv_obj_set_pos(s_lbl_speaking_title, 0, 0);
  lv_obj_update_layout(s_lbl_speaking_title);
  lv_coord_t th = lv_obj_get_height(s_lbl_speaking_title);
  lv_obj_set_pos(s_lbl_speaking_body, 0, th + 4);
  lv_obj_update_layout(s_lbl_speaking_body);
  lv_obj_update_layout(s_scroll_speaking_text);
}

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

static lv_coord_t text_width_px(const char* text, const lv_font_t* font) {
  lv_point_t size = {0};
  if (text == NULL || font == NULL) {
    return 0;
  }
  lv_text_get_size(&size, text, font, 0, 0, LV_COORD_MAX, LV_TEXT_FLAG_NONE);
  return size.x;
}

static void format_wifi_info(char* out, size_t out_size, const char* status) {
  if (out == NULL || out_size == 0) {
    return;
  }
  const char* ssid = bb_wifi_get_active_ssid();
  if (ssid != NULL && ssid[0] != '\0') {
    snprintf(out, out_size, "%s", ssid);
    return;
  }
  snprintf(out, out_size, "WiFi");
}

static int wifi_signal_level(const char* status) {
  if (status != NULL && strstr(status, "ERR") != NULL) {
    return 0;
  }
  int rssi = bb_wifi_get_rssi();
  if (rssi == 0) {
    return 1;  /* not connected */
  }
  if (rssi >= -50) return 4;
  if (rssi >= -65) return 3;
  if (rssi >= -80) return 2;
  return 1;
}

static void apply_standby_wifi_state(const char* status) {
  if (s_lbl_standby_wifi_info == NULL) {
    return;
  }

  const int level = wifi_signal_level(status);
  lv_color_t on = lv_color_hex(UI_ME_ACCENT);
  lv_color_t off = lv_color_hex(UI_TEXT_DIM);
  for (int i = 0; i < UI_WIFI_BAR_COUNT; ++i) {
    if (s_bar_standby_wifi[i] == NULL) {
      continue;
    }
    lv_obj_set_style_bg_color(s_bar_standby_wifi[i], i < level ? on : off, 0);
    lv_obj_set_style_bg_opa(s_bar_standby_wifi[i], i < level ? LV_OPA_COVER : LV_OPA_50, 0);
  }

  char wifi_info[64];
  format_wifi_info(wifi_info, sizeof(wifi_info), status);
  lv_label_set_text(s_lbl_standby_wifi_info, wifi_info);
}

static int line_px(void) {
  const lv_font_t* f = ui_font();
  /* 略紧的行距，多挤一行可见文字 */
  return (int)lv_font_get_line_height(f) + 1;
}

static int scaled_px(int px, int scale) {
  return (px * scale + 255) / 256;
}

static void set_speaking_bar_geometry(int index, int height) {
  if (index < 0 || index >= SPEAK_BAR_COUNT || s_bar_speaking[index] == NULL) {
    return;
  }
  lv_obj_set_size(s_bar_speaking[index], 6, height);
  lv_obj_set_pos(s_bar_speaking[index], s_speaking_bar_base_x + index * 10, s_speaking_bar_base_y - height);
}

static void speaking_anim_timer_cb(lv_timer_t* timer) {
  static const uint8_t tx_frames[][SPEAK_BAR_COUNT] = {
      {14, 28, 22, 36, 24, 18, 16}, {22, 38, 28, 44, 32, 26, 20}, {12, 22, 18, 32, 20, 16, 14},
      {18, 32, 26, 40, 30, 22, 18},
  };
  static const uint8_t rx_frames[][SPEAK_BAR_COUNT] = {
      {8, 12, 20, 14, 16, 12, 8}, {10, 16, 26, 18, 20, 14, 10}, {12, 18, 30, 22, 24, 16, 12},
      {10, 14, 24, 18, 20, 14, 10},
  };
  static const uint8_t speak_frames[][SPEAK_BAR_COUNT] = {
      {18, 26, 20, 34, 28, 22, 18}, {24, 34, 26, 40, 36, 28, 22}, {20, 28, 22, 36, 30, 24, 20},
      {14, 22, 18, 30, 26, 20, 16},
  };
  static const uint8_t idle_frame[SPEAK_BAR_COUNT] = {6, 10, 14, 18, 14, 10, 6};
  const uint8_t(*frames)[SPEAK_BAR_COUNT] = NULL;
  const uint8_t* heights = idle_frame;
  uint32_t frame_count = 0;

  (void)timer;

  switch (s_speaking_anim_mode) {
    case SPEAK_ANIM_TX:
      frames = tx_frames;
      frame_count = sizeof(tx_frames) / sizeof(tx_frames[0]);
      break;
    case SPEAK_ANIM_RX:
      frames = rx_frames;
      frame_count = sizeof(rx_frames) / sizeof(rx_frames[0]);
      break;
    case SPEAK_ANIM_SPEAK:
      frames = speak_frames;
      frame_count = sizeof(speak_frames) / sizeof(speak_frames[0]);
      break;
    case SPEAK_ANIM_IDLE:
    default:
      break;
  }

  if (frames != NULL && frame_count > 0) {
    heights = frames[s_speaking_anim_step % frame_count];
    s_speaking_anim_step++;
  }

  for (int i = 0; i < SPEAK_BAR_COUNT; ++i) {
    set_speaking_bar_geometry(i, heights[i]);
  }
}

static void set_speaking_anim_mode(speak_anim_mode_t mode) {
  if (s_speaking_anim_mode == mode && s_speaking_anim_timer != NULL) {
    return;
  }
  s_speaking_anim_mode = mode;
  s_speaking_anim_step = 0;
  if (s_speaking_anim_timer != NULL) {
    speaking_anim_timer_cb(s_speaking_anim_timer);
  }
}

static const char* chat_line_visible(const char* s) {
  return (s != NULL && s[0] != '\0') ? s : "--";
}

static int is_speaking_status(const char* status) {
  return status != NULL &&
         (strcmp(status, "TX") == 0 || strcmp(status, "RX") == 0 || strcmp(status, "SPEAK") == 0 ||
          strcmp(status, "TRANSCRIBING") == 0 || strcmp(status, "PROCESSING") == 0 || strcmp(status, "RESULT") == 0);
}

static int is_notification_status(const char* status) {
  return status != NULL && (strcmp(status, "TASK") == 0 || strcmp(status, "BUSY") == 0);
}

static void set_view_visible(lv_obj_t* obj, int visible) {
  if (obj == NULL) {
    return;
  }
  if (visible) {
    lv_obj_clear_flag(obj, LV_OBJ_FLAG_HIDDEN);
  } else {
    lv_obj_add_flag(obj, LV_OBJ_FLAG_HIDDEN);
  }
}

/** 与 bb_radio_app 状态字符串对应，图标源自 assets/svg 经脚本转 RGB565 */
static void apply_status_icon(const char* status) {
  const lv_image_dsc_t* src = &bb_img_ready;
  if (status == NULL || status[0] == '\0') {
    lv_image_set_src(s_img_status, src);
    return;
  }
  if (strstr(status, "ERR") != NULL) {
    src = &bb_img_err;
  } else if (strcmp(status, "TX") == 0) {
    src = &bb_img_tx;
  } else if (strcmp(status, "RX") == 0) {
    src = &bb_img_rx;
  } else if (strcmp(status, "TASK") == 0 || strcmp(status, "BUSY") == 0) {
    src = &bb_img_task;
  } else if (strcmp(status, "SPEAK") == 0) {
    src = &bb_img_speak;
  } else if (strcmp(status, "RESULT") == 0) {
    src = &bb_img_ready;
  } else if (strncmp(status, "BOOT", 4) == 0 || strstr(status, "WIFI") != NULL || strstr(status, "ADAPTER") != NULL ||
             strstr(status, "SPK") != NULL) {
    src = &bb_img_task;
  } else if (strcmp(status, "READY") == 0) {
    src = &bb_img_ready;
  }
  lv_image_set_src(s_img_status, src);
}

#if !defined(BBCLAW_SIMULATOR)
static void backlight_on(void) {
  gpio_config_t io_conf = {
      .pin_bit_mask = 1ULL << BBCLAW_ST7789_BL_GPIO,
      .mode = GPIO_MODE_OUTPUT,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  (void)gpio_config(&io_conf);
  (void)gpio_set_level(BBCLAW_ST7789_BL_GPIO, 1);
}

static esp_err_t init_panel(void) {
  const spi_host_device_t host = (spi_host_device_t)BBCLAW_ST7789_HOST;
  spi_bus_config_t buscfg = {
      .sclk_io_num = BBCLAW_ST7789_SCLK_GPIO,
      .mosi_io_num = BBCLAW_ST7789_MOSI_GPIO,
      .miso_io_num = -1,
      .quadwp_io_num = -1,
      .quadhd_io_num = -1,
      .max_transfer_sz = DISP_W * 64 * (int)sizeof(uint16_t),
  };
  esp_err_t err = spi_bus_initialize(host, &buscfg, SPI_DMA_CH_AUTO);
  if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
    return err;
  }

  esp_lcd_panel_io_spi_config_t io_config = {
      .dc_gpio_num = BBCLAW_ST7789_DC_GPIO,
      .cs_gpio_num = BBCLAW_ST7789_CS_GPIO,
      .pclk_hz = DISP_PCLK_HZ,
      .spi_mode = 0,
      .trans_queue_depth = 10,
      .lcd_cmd_bits = 8,
      .lcd_param_bits = 8,
  };
  ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)host, &io_config, &s_panel_io), TAG,
                      "new panel io failed");

  esp_lcd_panel_dev_config_t panel_config = {
      .reset_gpio_num = BBCLAW_ST7789_RST_GPIO,
      .rgb_ele_order = DISP_RGB_ORDER,
      .bits_per_pixel = 16,
  };
  ESP_RETURN_ON_ERROR(esp_lcd_new_panel_st7789(s_panel_io, &panel_config, &s_panel), TAG, "new st7789 panel failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_reset(s_panel), TAG, "panel reset failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_init(s_panel), TAG, "panel init failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_set_gap(s_panel, DISP_X_GAP, DISP_Y_GAP), TAG, "panel set gap failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_invert_color(s_panel, DISP_INVERT_COLOR), TAG, "panel invert failed");
  ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(s_panel, true), TAG, "panel on failed");
  return ESP_OK;
}

static void lvgl_flush_cb(lv_display_t* disp, const lv_area_t* area, uint8_t* color_map) {
  if (disp == NULL || area == NULL || color_map == NULL) {
    if (disp != NULL) {
      lvgl_port_flush_ready(disp);
    }
    return;
  }

#if DISP_SWAP_BYTES
  lv_draw_sw_rgb565_swap(color_map, lv_area_get_size(area));
#endif

  esp_err_t err = esp_lcd_panel_draw_bitmap(s_panel, area->x1, area->y1, area->x2 + 1, area->y2 + 1, color_map);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "panel draw failed: %s", esp_err_to_name(err));
    lvgl_port_flush_ready(disp);
  }
}
#endif

static void create_ui(void) {
  lv_obj_t* scr = lv_screen_active();
  lv_obj_set_style_bg_color(scr, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(scr, LV_OPA_COVER, 0);
  lv_obj_clear_flag(scr, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(scr, LV_SCROLLBAR_MODE_OFF);
  add_screen_grid_decor(scr);

  const lv_font_t* font = ui_font();
  const int lh = (int)lv_font_get_line_height(font);
  const int status_h = (lh + 2 > UI_STATUS_ICON_SZ + 2) ? (lh + 2) : (UI_STATUS_ICON_SZ + 2);
  int footer_h = lh > 14 ? lh - 1 : lh;
  if (footer_h < UI_FOOTER_LOGO_SZ + 2) {
    footer_h = UI_FOOTER_LOGO_SZ + 2;
  }
  const int gap = UI_GAP;
  const int frame_x = UI_SAFE_LEFT;
  const int top_y = UI_SAFE_TOP;
  const int content_y = top_y + status_h + gap;
  const int content_h = DISP_H - content_y - gap - footer_h - UI_SAFE_BOTTOM;
  const int body_w = DISP_W - UI_SAFE_LEFT - UI_SAFE_RIGHT;
  const int standby_hero_w = scaled_px(bb_el_claw_mark_88.header.w, UI_DECOR_SCALE_HERO);
  const int speaking_hero_w = scaled_px(bb_el_mic_plate_88.header.w, UI_DECOR_SCALE_HERO);
  const int speaking_hero_h = scaled_px(bb_el_mic_plate_88.header.h, UI_DECOR_SCALE_HERO);
  const int footer_logo_w = scaled_px(bb_el_claw_mark_88.header.w, UI_DECOR_SCALE_FOOTER);
  const int footer_logo_h = scaled_px(bb_el_claw_mark_88.header.h, UI_DECOR_SCALE_FOOTER);
  const int status_text_x = frame_x + UI_STATUS_ICON_SZ + 4;
  const int status_right_w = UI_WIFI_INFO_W;
  const int status_right_x = frame_x + body_w - status_right_w;
  const int status_label_w = body_w - (status_text_x - frame_x) - status_right_w - 8;
  const lv_coord_t standby_clock_w = body_w - 20;
  const int standby_clock_h = (int)lv_font_get_line_height(ui_font_clock());
  const int standby_wifi_y = top_y + (status_h - 16) / 2;
  const int standby_clock_y = 36;
  const int standby_body_top = standby_clock_y + standby_clock_h + 4;
  /* 底部 meta（uptime / time sync）占一行；原先 scroll 贴到 content_h-8，与 meta(y≈content_h-lh-6) 重叠，WiFi 未就绪时一直显示 meta，表现为文字叠在一起 */
  const int standby_meta_bottom_pad = lh + 12;

  s_img_status = lv_image_create(scr);
  lv_image_set_src(s_img_status, &bb_img_ready);
  lv_obj_set_size(s_img_status, UI_STATUS_ICON_SZ, UI_STATUS_ICON_SZ);
  lv_obj_set_pos(s_img_status, frame_x, top_y + (status_h - UI_STATUS_ICON_SZ) / 2);

  s_img_battery_frame = lv_image_create(scr);
  lv_image_set_src(s_img_battery_frame, &bb_el_battery_frame_26x12);
  lv_obj_set_pos(s_img_battery_frame, status_right_x, top_y + (status_h - bb_el_battery_frame_26x12.header.h) / 2);
  lv_obj_add_flag(s_img_battery_frame, LV_OBJ_FLAG_HIDDEN);

  s_lbl_status = lv_label_create(scr);
  lv_obj_set_width(s_lbl_status, status_label_w);
  lv_obj_set_style_text_color(s_lbl_status, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_text_font(s_lbl_status, font, 0);
  lv_label_set_long_mode(s_lbl_status, LV_LABEL_LONG_MODE_SCROLL_CIRCULAR);
  lv_obj_set_height(s_lbl_status, lh + 2);
  lv_label_set_text(s_lbl_status, "BOOT");
  lv_obj_set_pos(s_lbl_status, status_text_x, top_y + (status_h - lh - 2) / 2);

  s_lbl_turn_info = lv_label_create(scr);
  lv_obj_set_width(s_lbl_turn_info, 60);
  lv_obj_set_style_text_color(s_lbl_turn_info, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_lbl_turn_info, font, 0);
  lv_obj_set_style_text_align(s_lbl_turn_info, LV_TEXT_ALIGN_RIGHT, 0);
  lv_label_set_long_mode(s_lbl_turn_info, LV_LABEL_LONG_MODE_CLIP);
  lv_obj_set_height(s_lbl_turn_info, lh + 2);
  lv_label_set_text(s_lbl_turn_info, "");
  lv_obj_set_pos(s_lbl_turn_info, frame_x + body_w - 60, top_y + (status_h - lh - 2) / 2);
  lv_obj_add_flag(s_lbl_turn_info, LV_OBJ_FLAG_HIDDEN);

  s_view_standby = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view_standby);
  lv_obj_set_size(s_view_standby, body_w, content_h);
  lv_obj_set_pos(s_view_standby, frame_x, content_y);
  lv_obj_clear_flag(s_view_standby, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_standby, LV_SCROLLBAR_MODE_OFF);

  s_img_standby_hero = lv_image_create(s_view_standby);
  lv_image_set_src(s_img_standby_hero, &bb_el_claw_mark_88);
  lv_image_set_scale(s_img_standby_hero, UI_DECOR_SCALE_HERO);
  lv_obj_set_style_opa(s_img_standby_hero, LV_OPA_20, 0);
  lv_obj_set_pos(s_img_standby_hero, body_w - standby_hero_w + 18, content_h - standby_hero_w + 10);

  s_obj_standby_wifi = lv_obj_create(scr);
  lv_obj_remove_style_all(s_obj_standby_wifi);
  lv_obj_set_size(s_obj_standby_wifi, UI_WIFI_INFO_W, 16);
  lv_obj_set_pos(s_obj_standby_wifi, frame_x + body_w - UI_WIFI_INFO_W, standby_wifi_y);
  lv_obj_clear_flag(s_obj_standby_wifi, LV_OBJ_FLAG_SCROLLABLE);

  for (int i = 0; i < UI_WIFI_BAR_COUNT; ++i) {
    const int bar_h = (i + 1) * UI_WIFI_BAR_H_STEP;
    const int bars_x = UI_WIFI_INFO_W - (UI_WIFI_BAR_COUNT * UI_WIFI_BAR_W + (UI_WIFI_BAR_COUNT - 1) * UI_WIFI_BAR_GAP);
    s_bar_standby_wifi[i] = lv_obj_create(s_obj_standby_wifi);
    lv_obj_remove_style_all(s_bar_standby_wifi[i]);
    lv_obj_set_size(s_bar_standby_wifi[i], UI_WIFI_BAR_W, bar_h);
    lv_obj_set_style_radius(s_bar_standby_wifi[i], 1, 0);
    lv_obj_set_pos(s_bar_standby_wifi[i], bars_x + i * (UI_WIFI_BAR_W + UI_WIFI_BAR_GAP), 12 - bar_h);
  }

  s_lbl_standby_wifi_info = lv_label_create(s_obj_standby_wifi);
  lv_obj_set_width(s_lbl_standby_wifi_info, UI_WIFI_INFO_W - 22);
  lv_obj_set_style_text_color(s_lbl_standby_wifi_info, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_text_font(s_lbl_standby_wifi_info, font, 0);
  lv_obj_set_style_text_align(s_lbl_standby_wifi_info, LV_TEXT_ALIGN_LEFT, 0);
  lv_label_set_long_mode(s_lbl_standby_wifi_info, LV_LABEL_LONG_MODE_SCROLL_CIRCULAR);
  lv_label_set_text(s_lbl_standby_wifi_info, "WIFI");
  lv_obj_set_pos(s_lbl_standby_wifi_info, 0, 0);

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
    const lv_coord_t brand_w = logo_w + brand_gap + join_w + brand_gap + openclaw_w;
    const lv_coord_t brand_x = (body_w - brand_w) / 2;
    const lv_coord_t brand_y = 6;

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

  s_lbl_standby_clock = lv_label_create(s_view_standby);
  lv_obj_set_width(s_lbl_standby_clock, standby_clock_w);
  lv_obj_set_height(s_lbl_standby_clock, standby_clock_h + 2);
  lv_obj_set_style_bg_opa(s_lbl_standby_clock, LV_OPA_TRANSP, LV_PART_MAIN);
  lv_obj_set_style_text_color(s_lbl_standby_clock, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_text_font(s_lbl_standby_clock, ui_font_clock(), 0);
  lv_obj_set_style_text_align(s_lbl_standby_clock, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_style_text_letter_space(s_lbl_standby_clock, 1, 0);
  lv_label_set_long_mode(s_lbl_standby_clock, LV_LABEL_LONG_MODE_CLIP);
  lv_label_set_text(s_lbl_standby_clock, "--:--");
  lv_obj_set_pos(s_lbl_standby_clock, (body_w - standby_clock_w) / 2, standby_clock_y);
  s_standby_clock_base_y = standby_clock_y;

  {
    lv_anim_t a;
    lv_anim_init(&a);
    lv_anim_set_var(&a, s_lbl_standby_clock);
    lv_anim_set_values(&a, standby_clock_y, standby_clock_y - 3);
    lv_anim_set_duration(&a, 1200);
    lv_anim_set_playback_duration(&a, 1200);
    lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
    lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
    lv_anim_set_exec_cb(&a, standby_clock_anim_y_cb);
    lv_anim_start(&a);
  }

  {
    const int scroll_h = content_h - standby_body_top - standby_meta_bottom_pad;
    s_scroll_standby_body = lv_obj_create(s_view_standby);
    lv_obj_remove_style_all(s_scroll_standby_body);
    lv_obj_set_size(s_scroll_standby_body, body_w - 36, scroll_h > 28 ? scroll_h : 28);
    lv_obj_set_pos(s_scroll_standby_body, 18, standby_body_top);
    lv_obj_add_flag(s_scroll_standby_body, LV_OBJ_FLAG_SCROLLABLE);
    lv_obj_set_scrollbar_mode(s_scroll_standby_body, LV_SCROLLBAR_MODE_OFF);
    lv_obj_set_scroll_dir(s_scroll_standby_body, LV_DIR_VER);
    s_lbl_standby_body = lv_label_create(s_scroll_standby_body);
    lv_obj_set_width(s_lbl_standby_body, body_w - 44);
    lv_label_set_long_mode(s_lbl_standby_body, LV_LABEL_LONG_MODE_WRAP);
    lv_obj_set_style_text_color(s_lbl_standby_body, lv_color_hex(UI_TEXT_MAIN), 0);
    lv_obj_set_style_text_font(s_lbl_standby_body, font, 0);
    lv_obj_set_style_text_align(s_lbl_standby_body, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_lbl_standby_body, "Waiting for PTT.");
    lv_obj_set_pos(s_lbl_standby_body, 0, 0);
  }

  s_lbl_standby_meta = lv_label_create(s_view_standby);
  lv_obj_set_width(s_lbl_standby_meta, body_w - 8);
  lv_obj_set_style_text_color(s_lbl_standby_meta, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_lbl_standby_meta, font, 0);
  lv_obj_set_style_text_align(s_lbl_standby_meta, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_long_mode(s_lbl_standby_meta, LV_LABEL_LONG_MODE_CLIP);
  lv_obj_set_height(s_lbl_standby_meta, lh + 2);
  lv_label_set_text(s_lbl_standby_meta, "");
  lv_obj_set_pos(s_lbl_standby_meta, 4, content_h - lh - 8);

  s_view_notification = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view_notification);
  lv_obj_set_size(s_view_notification, body_w, content_h);
  lv_obj_set_pos(s_view_notification, frame_x, content_y);
  lv_obj_clear_flag(s_view_notification, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_notification, LV_SCROLLBAR_MODE_OFF);

  {
    lv_obj_t* card = lv_obj_create(s_view_notification);
    lv_obj_remove_style_all(card);
    lv_obj_set_size(card, body_w, content_h - 2);
    lv_obj_set_pos(card, 0, 0);
    lv_obj_clear_flag(card, LV_OBJ_FLAG_SCROLLABLE);
    lv_obj_set_scrollbar_mode(card, LV_SCROLLBAR_MODE_OFF);

    {
      const int card_h = content_h - 2;
      const int nfy_text_top = 24;
      const int nfy_footer_y = card_h - UI_NOTIFY_FOOTER_ROW - 2;
      const int nfy_text_h = nfy_footer_y - nfy_text_top - 10;
      const int nfy_pad = 18;
      const int nfy_action_w = 88;
      const int nfy_action_gap = 8;
      const int nfy_action_x = (body_w - (nfy_action_w * 2 + nfy_action_gap)) / 2;

      s_obj_notification_status_dot = lv_obj_create(card);
      lv_obj_remove_style_all(s_obj_notification_status_dot);
      lv_obj_set_size(s_obj_notification_status_dot, 5, 5);
      lv_obj_add_flag(s_obj_notification_status_dot, LV_OBJ_FLAG_HIDDEN);

      s_lbl_notification_task = lv_label_create(card);
      lv_obj_set_width(s_lbl_notification_task, body_w - 64);
      lv_obj_set_style_text_color(s_lbl_notification_task, lv_color_hex(UI_NOTIFY_TEXT_GRAY), 0);
      lv_obj_set_style_text_font(s_lbl_notification_task, font, 0);
      lv_label_set_long_mode(s_lbl_notification_task, LV_LABEL_LONG_MODE_CLIP);
      lv_obj_set_height(s_lbl_notification_task, lh + 1);
      lv_label_set_text(s_lbl_notification_task, "");
      lv_obj_add_flag(s_lbl_notification_task, LV_OBJ_FLAG_HIDDEN);

      s_lbl_notification_new = lv_label_create(card);
      lv_label_set_text(s_lbl_notification_new, "");
      lv_obj_add_flag(s_lbl_notification_new, LV_OBJ_FLAG_HIDDEN);

      s_img_notification_avatar = lv_image_create(card);
      lv_image_set_src(s_img_notification_avatar, &bb_el_avatar_plate_64);
      lv_obj_add_flag(s_img_notification_avatar, LV_OBJ_FLAG_HIDDEN);

      s_scroll_notification_text = lv_obj_create(card);
      lv_obj_remove_style_all(s_scroll_notification_text);
      lv_obj_set_size(s_scroll_notification_text, body_w - nfy_pad * 2, nfy_text_h > 44 ? nfy_text_h : 44);
      lv_obj_set_pos(s_scroll_notification_text, nfy_pad, nfy_text_top);
      lv_obj_add_flag(s_scroll_notification_text, LV_OBJ_FLAG_SCROLLABLE);
      lv_obj_set_scroll_dir(s_scroll_notification_text, LV_DIR_VER);
      lv_obj_set_scrollbar_mode(s_scroll_notification_text, LV_SCROLLBAR_MODE_OFF);

      s_lbl_notification_title = lv_label_create(s_scroll_notification_text);
      lv_obj_set_style_text_color(s_lbl_notification_title, lv_color_hex(UI_NOTIFY_TEXT_WHITE), 0);
      lv_obj_set_style_text_font(s_lbl_notification_title, font, 0);
      lv_obj_set_style_text_align(s_lbl_notification_title, LV_TEXT_ALIGN_LEFT, 0);
      lv_label_set_text(s_lbl_notification_title, "Alice");

      s_lbl_notification_body = lv_label_create(s_scroll_notification_text);
      lv_obj_set_style_text_color(s_lbl_notification_body, lv_color_hex(UI_NOTIFY_TEXT_GRAY), 0);
      lv_obj_set_style_text_font(s_lbl_notification_body, font, 0);
      lv_obj_set_style_text_align(s_lbl_notification_body, LV_TEXT_ALIGN_LEFT, 0);
      lv_label_set_text(s_lbl_notification_body, "--");

      s_obj_notification_divider = lv_obj_create(card);
      lv_obj_remove_style_all(s_obj_notification_divider);
      lv_obj_add_flag(s_obj_notification_divider, LV_OBJ_FLAG_HIDDEN);

      s_lbl_notification_open = lv_label_create(card);
      lv_obj_set_width(s_lbl_notification_open, nfy_action_w);
      lv_obj_set_style_text_color(s_lbl_notification_open, lv_color_hex(UI_NOTIFY_BORDER), 0);
      lv_obj_set_style_text_font(s_lbl_notification_open, font, 0);
      lv_obj_set_style_text_align(s_lbl_notification_open, LV_TEXT_ALIGN_CENTER, 0);
      lv_label_set_long_mode(s_lbl_notification_open, LV_LABEL_LONG_MODE_CLIP);
      lv_obj_set_height(s_lbl_notification_open, lh + 1);
      lv_label_set_text(s_lbl_notification_open, "OPEN");
      lv_obj_set_pos(s_lbl_notification_open, nfy_action_x, nfy_footer_y + 1);

      s_lbl_notification_dismiss = lv_label_create(card);
      lv_obj_set_width(s_lbl_notification_dismiss, nfy_action_w);
      lv_obj_set_style_text_color(s_lbl_notification_dismiss, lv_color_hex(UI_TEXT_DIM), 0);
      lv_obj_set_style_text_font(s_lbl_notification_dismiss, font, 0);
      lv_obj_set_style_text_align(s_lbl_notification_dismiss, LV_TEXT_ALIGN_CENTER, 0);
      lv_label_set_long_mode(s_lbl_notification_dismiss, LV_LABEL_LONG_MODE_CLIP);
      lv_obj_set_height(s_lbl_notification_dismiss, lh + 1);
      lv_label_set_text(s_lbl_notification_dismiss, "DISMISS");
      lv_obj_set_pos(s_lbl_notification_dismiss, nfy_action_x + nfy_action_w + nfy_action_gap, nfy_footer_y + 1);
    }
  }

  s_view_speaking = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view_speaking);
  lv_obj_set_size(s_view_speaking, body_w, content_h);
  lv_obj_set_pos(s_view_speaking, frame_x, content_y);
  lv_obj_clear_flag(s_view_speaking, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view_speaking, LV_SCROLLBAR_MODE_OFF);

  {
    lv_obj_t* card = lv_obj_create(s_view_speaking);
    lv_obj_remove_style_all(card);
    lv_obj_set_size(card, body_w, content_h - 2);
    lv_obj_set_pos(card, 0, 0);
    lv_obj_clear_flag(card, LV_OBJ_FLAG_SCROLLABLE);
    lv_obj_set_scrollbar_mode(card, LV_SCROLLBAR_MODE_OFF);

    {
      const int sp_ch = content_h - 2;
      const int sp_pad = 18;
      const int sp_text_left = sp_pad;
      const int sp_text_w = body_w - sp_pad * 2;
      const int sp_top = 24;
      const int sp_bar_strip = 32;
      const int sp_wave_w = (SPEAK_BAR_COUNT - 1) * 10 + 6;

      s_img_speaking_hero = lv_image_create(card);
      lv_image_set_src(s_img_speaking_hero, &bb_el_mic_plate_88);
      lv_image_set_scale(s_img_speaking_hero, UI_DECOR_SCALE_HERO);
      lv_obj_set_style_opa(s_img_speaking_hero, LV_OPA_20, 0);
      lv_obj_set_pos(s_img_speaking_hero, (body_w - speaking_hero_w) / 2, sp_ch - speaking_hero_h + 8);

      s_lbl_ptt_indicator = lv_label_create(card);
      lv_label_set_text(s_lbl_ptt_indicator, "");
      lv_obj_add_flag(s_lbl_ptt_indicator, LV_OBJ_FLAG_HIDDEN);

      s_speaking_bar_base_y = sp_ch - 18;
      s_speaking_bar_base_x = (body_w - sp_wave_w) / 2;
      int sp_scroll_h = s_speaking_bar_base_y - sp_top - sp_bar_strip;
      if (sp_scroll_h < 32) {
        sp_scroll_h = 32;
      }

      s_scroll_speaking_text = lv_obj_create(card);
      lv_obj_remove_style_all(s_scroll_speaking_text);
      lv_obj_set_size(s_scroll_speaking_text, sp_text_w, sp_scroll_h);
      lv_obj_set_pos(s_scroll_speaking_text, sp_text_left, sp_top);
      lv_obj_add_flag(s_scroll_speaking_text, LV_OBJ_FLAG_SCROLLABLE);
      lv_obj_set_scroll_dir(s_scroll_speaking_text, LV_DIR_VER);
      lv_obj_set_scrollbar_mode(s_scroll_speaking_text, LV_SCROLLBAR_MODE_OFF);

      s_lbl_speaking_title = lv_label_create(s_scroll_speaking_text);
      lv_obj_set_style_text_color(s_lbl_speaking_title, lv_color_hex(UI_NOTIFY_TEXT_WHITE), 0);
      lv_obj_set_style_text_font(s_lbl_speaking_title, font, 0);
      lv_obj_set_style_text_align(s_lbl_speaking_title, LV_TEXT_ALIGN_LEFT, 0);
      lv_label_set_text(s_lbl_speaking_title, "Listening");

      s_lbl_speaking_body = lv_label_create(s_scroll_speaking_text);
      lv_obj_set_style_text_color(s_lbl_speaking_body, lv_color_hex(UI_NOTIFY_TEXT_GRAY), 0);
      lv_obj_set_style_text_font(s_lbl_speaking_body, font, 0);
      lv_obj_set_style_text_align(s_lbl_speaking_body, LV_TEXT_ALIGN_LEFT, 0);
      lv_label_set_text(s_lbl_speaking_body, "Hold PTT and talk.");
    }
    for (int i = 0; i < SPEAK_BAR_COUNT; ++i) {
      s_bar_speaking[i] = lv_obj_create(card);
      lv_obj_remove_style_all(s_bar_speaking[i]);
      lv_obj_set_style_bg_color(s_bar_speaking[i], lv_color_hex(UI_ME_ACCENT), 0);
      lv_obj_set_style_bg_opa(s_bar_speaking[i], LV_OPA_70, 0);
      lv_obj_set_style_radius(s_bar_speaking[i], 2, 0);
      lv_obj_set_size(s_bar_speaking[i], 6, 16);
      set_speaking_bar_geometry(i, 16);
    }

    s_lbl_speaking_hint = lv_label_create(card);
    lv_obj_set_width(s_lbl_speaking_hint, 108);
    lv_label_set_long_mode(s_lbl_speaking_hint, LV_LABEL_LONG_MODE_CLIP);
    lv_obj_set_height(s_lbl_speaking_hint, lh + 1);
    lv_obj_set_style_text_color(s_lbl_speaking_hint, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_speaking_hint, font, 0);
    lv_obj_set_style_text_align(s_lbl_speaking_hint, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_text(s_lbl_speaking_hint, "hold to speak");
    lv_obj_set_pos(s_lbl_speaking_hint, (body_w - 108) / 2, s_speaking_bar_base_y + 2);
  }

  {
    const int footer_y = DISP_H - UI_SAFE_BOTTOM - footer_h;
    s_img_footer_logo = lv_image_create(scr);
    lv_image_set_src(s_img_footer_logo, &bb_el_claw_mark_88);
    lv_image_set_scale(s_img_footer_logo, UI_DECOR_SCALE_FOOTER);
    lv_obj_set_pos(s_img_footer_logo, frame_x, footer_y + (footer_h - footer_logo_h) / 2);

    s_lbl_footer_wifi = lv_label_create(scr);
    lv_obj_set_width(s_lbl_footer_wifi, body_w / 2);
    lv_obj_set_style_text_color(s_lbl_footer_wifi, lv_color_hex(UI_STATUS_FG), 0);
    lv_obj_set_style_text_font(s_lbl_footer_wifi, font, 0);
    lv_label_set_text(s_lbl_footer_wifi, "WIFI");
    lv_obj_set_pos(s_lbl_footer_wifi, frame_x + footer_logo_w + 6, footer_y + (footer_h - lh) / 2);

    s_lbl_footer_state = lv_label_create(scr);
    lv_obj_set_width(s_lbl_footer_state, body_w / 2);
    lv_obj_set_style_text_color(s_lbl_footer_state, lv_color_hex(UI_STATUS_FG), 0);
    lv_obj_set_style_text_font(s_lbl_footer_state, font, 0);
    lv_obj_set_style_text_align(s_lbl_footer_state, LV_TEXT_ALIGN_RIGHT, 0);
    lv_label_set_text(s_lbl_footer_state, "IDLE");
    lv_obj_set_pos(s_lbl_footer_state, frame_x + body_w / 2, footer_y + (footer_h - lh) / 2);

    s_lbl_footer = lv_label_create(scr);
    lv_obj_set_width(s_lbl_footer, body_w - footer_logo_w - 6);
    lv_obj_set_style_text_color(s_lbl_footer, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_text_font(s_lbl_footer, font, 0);
    lv_obj_set_style_text_align(s_lbl_footer, LV_TEXT_ALIGN_RIGHT, 0);
    lv_label_set_text(s_lbl_footer, "--");
    lv_obj_set_pos(s_lbl_footer, frame_x + footer_logo_w + 6, footer_y + (footer_h - lh) / 2);
    lv_obj_add_flag(s_lbl_footer, LV_OBJ_FLAG_HIDDEN);
  }

  set_view_visible(s_view_standby, 1);
  set_view_visible(s_view_notification, 0);
  set_view_visible(s_view_speaking, 0);
  auto_scroll_ctx_attach(&s_auto_scroll_standby, s_scroll_standby_body);
  auto_scroll_ctx_attach(&s_auto_scroll_notification, s_scroll_notification_text);
  auto_scroll_ctx_attach(&s_auto_scroll_speaking, s_scroll_speaking_text);
  s_speaking_anim_timer = lv_timer_create(speaking_anim_timer_cb, 72, NULL);
  set_speaking_anim_mode(SPEAK_ANIM_IDLE);
  s_clock_timer = lv_timer_create(clock_timer_cb, 1000, NULL);
  s_auto_scroll_timer = lv_timer_create(auto_scroll_text_cb, UI_AUTO_SCROLL_PERIOD_MS, NULL);
}

static void refresh_ui(void) {
  char status[sizeof(s_status)];
  char you[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  char reply[BBCLAW_DISPLAY_CHAT_LINE_LEN];
  int turn_num = 0;
  int turn_den = 0;
  int64_t notification_until_ms = 0;

  portENTER_CRITICAL(&s_state_lock);
  memcpy(status, s_status, sizeof(status));
  turn_den = s_history_count;
  notification_until_ms = s_notification_until_ms;
  if (s_history_count <= 0) {
    you[0] = '\0';
    reply[0] = '\0';
  } else {
    int idx = s_history_count - 1 - s_view_back;
    if (idx < 0) {
      idx = 0;
    }
    memcpy(you, s_history[idx].you, sizeof(you));
    memcpy(reply, s_history[idx].reply, sizeof(reply));
    turn_num = s_history_count - s_view_back;
    you[sizeof(you) - 1] = '\0';
    reply[sizeof(reply) - 1] = '\0';
  }
  portEXIT_CRITICAL(&s_state_lock);
        
  /* 长文 + CJK 换行时 LVGL 可能持锁数百 ms；短超时会导致整次刷新被跳过（界面卡住只显示旧内容） */
  if (!lvgl_port_lock(0)) {
    return;
  }

  ui_view_mode_t mode = UI_VIEW_STANDBY;
  {
    const int64_t now_ms = bb_now_ms();
    const int uptime_sec = (int)(now_ms / 1000);
    const int uptime_min = uptime_sec / 60;
    const int uptime_rem = uptime_sec % 60;
    char buf[BBCLAW_DISPLAY_CHAT_LINE_LEN + 64];
    char hm[8];

    mode = resolve_view_mode(status, turn_den, notification_until_ms, now_ms);

    if (mode == UI_VIEW_SPEAKING && strcmp(status, "TX") == 0) {
      lv_label_set_text(s_lbl_status, "LISTENING");
    } else {
      lv_label_set_text(s_lbl_status, status[0] != '\0' ? status : "READY");
    }
    apply_status_icon(status);
    apply_standby_wifi_state(status);

    set_view_visible(s_view_standby, mode == UI_VIEW_STANDBY);
    set_view_visible(s_view_notification, mode == UI_VIEW_NOTIFICATION);
    set_view_visible(s_view_speaking, mode == UI_VIEW_SPEAKING);
    set_view_visible(s_obj_standby_wifi, mode == UI_VIEW_STANDBY);
    if (mode == UI_VIEW_STANDBY) {
      set_view_visible(s_lbl_turn_info, 0);
    } else if (turn_den > 0) {
      snprintf(buf, sizeof(buf), "%d/%d", turn_num, turn_den);
      lv_label_set_text(s_lbl_turn_info, buf);
      set_view_visible(s_lbl_turn_info, 1);
    } else {
      set_view_visible(s_lbl_turn_info, 0);
    }

    format_standby_clock(hm, sizeof(hm), now_ms);
    lv_label_set_text(s_lbl_standby_clock, hm);
    if (bb_wall_time_ready()) {
      lv_label_set_text(s_lbl_standby_meta, "");
    } else {
      snprintf(buf, sizeof(buf), "uptime %02d:%02d · time sync…", uptime_min, uptime_rem);
      lv_label_set_text(s_lbl_standby_meta, buf);
    }
    if (turn_den > 0) {
      snprintf(buf, sizeof(buf), "Recent reply ready. %d turn%s waiting below.", turn_den, turn_den > 1 ? "s" : "");
      lv_label_set_text(s_lbl_standby_body, buf);
    } else {
      lv_label_set_text(s_lbl_standby_body, "Press PTT to start talking.");
    }

    if (is_notification_status(status)) {
      lv_label_set_text(s_lbl_notification_title, "");
      if (reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "ME> %s\nAI> %s", chat_line_visible(you), reply);
      } else if (you[0] != '\0') {
        snprintf(buf, sizeof(buf), "ME> %s", you);
      } else {
        snprintf(buf, sizeof(buf), "> Pending task");
      }
      lv_label_set_text(s_lbl_notification_body, buf);
    } else {
      snprintf(buf, sizeof(buf), "ME> %s", chat_line_visible(you));
      lv_label_set_text(s_lbl_notification_title, buf);
      if (reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "AI> %s", reply);
      } else {
        buf[0] = '\0';
      }
      lv_label_set_text(s_lbl_notification_body, buf);
      lv_label_set_text(s_lbl_notification_task, "MSG");
    }
    if (turn_den > 0) {
      snprintf(buf, sizeof(buf), "OPEN  %d/%d", turn_num, turn_den);
    } else {
      snprintf(buf, sizeof(buf), "OPEN");
    }
    lv_label_set_text(s_lbl_notification_open, buf);
    lv_label_set_text(s_lbl_notification_dismiss, "DISMISS");

    if (strcmp(status, "TX") == 0) {
      lv_label_set_text(s_lbl_speaking_title, "> Listening...");
      lv_label_set_text(s_lbl_speaking_body, "");
      lv_label_set_text(s_lbl_speaking_hint, "release to send");
      set_speaking_anim_mode(SPEAK_ANIM_TX);
      set_view_visible(s_lbl_ptt_indicator, true);
    } else if (strcmp(status, "RX") == 0 || strcmp(status, "TRANSCRIBING") == 0 || strcmp(status, "PROCESSING") == 0) {
      if (you[0] != '\0') {
        snprintf(buf, sizeof(buf), "ME> %s", you);
        lv_label_set_text(s_lbl_speaking_title, buf);
      } else {
        lv_label_set_text(s_lbl_speaking_title, "> Processing...");
      }
      if (reply[0] != '\0') {
        snprintf(buf, sizeof(buf), "AI> %s", reply);
        lv_label_set_text(s_lbl_speaking_body, buf);
      } else {
        lv_label_set_text(s_lbl_speaking_body, "AI> ...");
      }
      lv_label_set_text(s_lbl_speaking_hint, "");
      set_speaking_anim_mode(SPEAK_ANIM_RX);
      set_view_visible(s_lbl_ptt_indicator, false);
    } else if (strcmp(status, "RESULT") == 0) {
      snprintf(buf, sizeof(buf), "ME> %s", you[0] != '\0' ? you : "(no speech)");
      lv_label_set_text(s_lbl_speaking_title, buf);
      snprintf(buf, sizeof(buf), "AI> %s", reply[0] != '\0' ? reply : "(no reply)");
      lv_label_set_text(s_lbl_speaking_body, buf);
      lv_label_set_text(s_lbl_speaking_hint, "");
      set_speaking_anim_mode(SPEAK_ANIM_IDLE);
      set_view_visible(s_lbl_ptt_indicator, false);
    } else {
      lv_label_set_text(s_lbl_speaking_title, "Reply playing");
      lv_label_set_text(s_lbl_speaking_body, "Reply playback is active on the desk unit.");
      lv_label_set_text(s_lbl_speaking_hint, "speaker on");
      set_speaking_anim_mode(mode == 2 ? SPEAK_ANIM_SPEAK : SPEAK_ANIM_IDLE);
      set_view_visible(s_lbl_ptt_indicator, false);
    }

    relayout_notification_text();
    relayout_speaking_text();
    if (s_main_text_scroll_dirty || mode != s_last_visible_mode) {
      s_last_visible_mode = mode;
      s_main_text_scroll_dirty = 0;
      auto_scroll_ctx_reset(&s_auto_scroll_standby);
      auto_scroll_ctx_reset(&s_auto_scroll_notification);
      auto_scroll_ctx_reset(&s_auto_scroll_speaking);
    }
  }

  set_view_visible(s_img_footer_logo, false);
  if (mode == UI_VIEW_STANDBY) {
    set_view_visible(s_lbl_footer_wifi, false);
    set_view_visible(s_lbl_footer_state, true);
    set_view_visible(s_lbl_footer, false);
    lv_label_set_text(s_lbl_footer_state, "IDLE");
  } else {
    set_view_visible(s_lbl_footer_wifi, false);
    set_view_visible(s_lbl_footer_state, false);
    set_view_visible(s_lbl_footer, false);
  }

  lvgl_port_unlock();
}

esp_err_t bb_display_init(void) {
#if defined(BBCLAW_SIMULATOR)
  strncpy(s_status, "BOOT", sizeof(s_status) - 1);
  s_status[sizeof(s_status) - 1] = '\0';
  s_history_count = 0;
  s_stream_turn_active = 0;
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_focus_ai = 1;
  s_notification_until_ms = 0;
  s_speaking_anim_timer = NULL;
  s_clock_timer = NULL;
  s_auto_scroll_timer = NULL;
  s_main_text_scroll_dirty = 0;
  s_last_visible_mode = -1;
  s_speaking_anim_mode = SPEAK_ANIM_IDLE;
  s_speaking_anim_step = 0;
  s_auto_scroll_pause_until_ms = 0;
  s_notification_scroll_complete = 1;
  memset(&s_auto_scroll_standby, 0, sizeof(s_auto_scroll_standby));
  memset(&s_auto_scroll_notification, 0, sizeof(s_auto_scroll_notification));
  memset(&s_auto_scroll_speaking, 0, sizeof(s_auto_scroll_speaking));

  create_ui();
  s_ready = 1;
  refresh_ui();
  return ESP_OK;
#else
  backlight_on();
  strncpy(s_status, "BOOT", sizeof(s_status) - 1);
  s_status[sizeof(s_status) - 1] = '\0';
  s_history_count = 0;
  s_stream_turn_active = 0;
  s_view_back = 0;
  s_scroll_you = 0;
  s_scroll_ai = 0;
  s_focus_ai = 1;
  s_notification_until_ms = 0;
  s_speaking_anim_timer = NULL;
  s_clock_timer = NULL;
  s_auto_scroll_timer = NULL;
  s_main_text_scroll_dirty = 0;
  s_last_visible_mode = -1;
  s_speaking_anim_mode = SPEAK_ANIM_IDLE;
  s_speaking_anim_step = 0;
  s_auto_scroll_pause_until_ms = 0;
  s_notification_scroll_complete = 1;
  memset(&s_auto_scroll_standby, 0, sizeof(s_auto_scroll_standby));
  memset(&s_auto_scroll_notification, 0, sizeof(s_auto_scroll_notification));
  memset(&s_auto_scroll_speaking, 0, sizeof(s_auto_scroll_speaking));

  ESP_RETURN_ON_ERROR(init_panel(), TAG, "panel init failed");

  lvgl_port_cfg_t lvgl_cfg = ESP_LVGL_PORT_INIT_CONFIG();
#if CONFIG_SOC_CPU_CORES_NUM > 1
  /* 与 bb_radio_app 中音频任务（固定 core0）分离，降低 SPI 刷屏与 I2S/环缓冲并发时 INT WDT 风险 */
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
      .rotation =
          {
              .swap_xy = (bool)DISP_SWAP_XY,
              .mirror_x = (bool)DISP_MIRROR_X,
              .mirror_y = (bool)DISP_MIRROR_Y,
          },
      .rounder_cb = NULL,
      .color_format = LV_COLOR_FORMAT_RGB565,
      .flags =
          {
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

  if (!lvgl_port_lock(pdMS_TO_TICKS(2000))) {
    return ESP_ERR_TIMEOUT;
  }
  create_ui();
  lvgl_port_unlock();

  s_ready = 1;
  ESP_LOGI(TAG, "lvgl display ready %dx%d font=%s rgb=%s swap_xy=%d mirror=(%d,%d) gap=(%d,%d) invert=%d",
           DISP_W, DISP_H,
#ifdef BBCLAW_HAVE_CJK_FONT
           "bbclaw_cjk",
#else
           "default",
#endif
           DISP_RGB_ORDER_NAME, DISP_SWAP_XY, DISP_MIRROR_X, DISP_MIRROR_Y, DISP_X_GAP, DISP_Y_GAP,
           DISP_INVERT_COLOR);
  refresh_ui();
  return ESP_OK;
#endif
}

esp_err_t bb_display_show_status(const char* status_line) {
  int notification_status = 0;
  if (status_line != NULL) {
    notification_status = is_notification_status(status_line);
    portENTER_CRITICAL(&s_state_lock);
    strncpy(s_status, status_line, sizeof(s_status) - 1);
    s_status[sizeof(s_status) - 1] = '\0';
    if (notification_status) {
      s_notification_until_ms = bb_now_ms() + UI_NOTIFICATION_MIN_MS;
      s_notification_scroll_complete = 0;
    }
    portEXIT_CRITICAL(&s_state_lock);
  }
  if (s_ready) {
    refresh_ui();
  }
  return ESP_OK;
}

esp_err_t bb_display_show_chat_turn(const char* user_said, const char* assistant_reply) {
  return bb_display_upsert_chat_turn(user_said, assistant_reply, 1);
}

esp_err_t bb_display_upsert_chat_turn(const char* user_said, const char* assistant_reply, int finalize) {
  const char* u = user_said != NULL ? user_said : "";
  const char* r = assistant_reply != NULL ? assistant_reply : "";
  if (u[0] == '\0' && r[0] == '\0') {
    return ESP_OK;
  }
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
  s_notification_until_ms = bb_now_ms() + UI_NOTIFICATION_MIN_MS;
  s_notification_scroll_complete = 0;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) {
    s_main_text_scroll_dirty = 1;
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
    s_notification_until_ms = bb_now_ms() + UI_NOTIFICATION_MIN_MS;
    s_notification_scroll_complete = 0;
    scroll_reset = 1;
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) {
    if (scroll_reset) {
      s_main_text_scroll_dirty = 1;
    }
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
    s_notification_until_ms = bb_now_ms() + UI_NOTIFICATION_MIN_MS;
    s_notification_scroll_complete = 0;
    scroll_reset = 1;
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) {
    if (scroll_reset) {
      s_main_text_scroll_dirty = 1;
    }
    refresh_ui();
  }
  return ESP_OK;
}

esp_err_t bb_display_chat_scroll_down(void) {
  char status[sizeof(s_status)];
  int turn_den = 0;
  int64_t notification_until_ms = 0;
  const int step = line_px() * UI_MANUAL_SCROLL_STEP_LINES;
  const int64_t now_ms = bb_now_ms();

  portENTER_CRITICAL(&s_state_lock);
  memcpy(status, s_status, sizeof(status));
  turn_den = s_history_count;
  notification_until_ms = s_notification_until_ms;
  if (s_focus_ai) {
    s_scroll_ai++;
  } else {
    s_scroll_you++;
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready && lvgl_port_lock(pdMS_TO_TICKS(200))) {
    const ui_view_mode_t mode = resolve_view_mode(status, turn_den, notification_until_ms, now_ms);
    lv_obj_t* cont = active_scroll_cont_for_mode(mode);
    ui_auto_scroll_ctx_t* ctx = auto_scroll_ctx_for_mode(mode);
    if (cont != NULL) {
      lv_obj_update_layout(cont);
      lv_obj_scroll_by_bounded(cont, 0, step, LV_ANIM_OFF);
      auto_scroll_ctx_note_manual(ctx);
      s_auto_scroll_pause_until_ms = now_ms + UI_MANUAL_SCROLL_PAUSE_MS;
    }
    lvgl_port_unlock();
  }
  return ESP_OK;
}

esp_err_t bb_display_chat_scroll_up(void) {
  char status[sizeof(s_status)];
  int turn_den = 0;
  int64_t notification_until_ms = 0;
  const int step = line_px() * UI_MANUAL_SCROLL_STEP_LINES;
  const int64_t now_ms = bb_now_ms();

  portENTER_CRITICAL(&s_state_lock);
  memcpy(status, s_status, sizeof(status));
  turn_den = s_history_count;
  notification_until_ms = s_notification_until_ms;
  if (s_focus_ai) {
    if (s_scroll_ai > 0) {
      s_scroll_ai--;
    }
  } else {
    if (s_scroll_you > 0) {
      s_scroll_you--;
    }
  }
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready && lvgl_port_lock(pdMS_TO_TICKS(200))) {
    const ui_view_mode_t mode = resolve_view_mode(status, turn_den, notification_until_ms, now_ms);
    lv_obj_t* cont = active_scroll_cont_for_mode(mode);
    ui_auto_scroll_ctx_t* ctx = auto_scroll_ctx_for_mode(mode);
    if (cont != NULL) {
      lv_obj_update_layout(cont);
      lv_obj_scroll_by_bounded(cont, 0, -step, LV_ANIM_OFF);
      auto_scroll_ctx_note_manual(ctx);
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
  if (s_ready) {
    refresh_ui();
  }
}

void bb_display_chat_focus_ai(void) {
  portENTER_CRITICAL(&s_state_lock);
  s_focus_ai = 1;
  portEXIT_CRITICAL(&s_state_lock);
  if (s_ready) {
    refresh_ui();
  }
}
