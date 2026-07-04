/**
 * bb_touch_input.c — FT5x06 兼容触控（手表 FT3168）→ 导航事件注入。
 *
 * 设计（ADR-040 §5 第二阶段）：不做 LVGL 指针 indev——BBClaw UI 是按键导航
 * 语义（固定页/行选中，ADR-012），把手势翻成 nav 事件比引入指针点击模型
 * 改动小得多，且所有页面立即可用。
 */
#include "bb_touch_input.h"

#include "bb_config.h"

#if BBCLAW_TOUCH_FT5X06_ENABLE

#include <stdlib.h>

#include "bb_audio.h"
#include "bb_nav_input.h"
#include "driver/i2c_master.h"
#include "esp_check.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_touch_ft5x06.h"
#include "esp_log.h"
#include "esp_timer.h"

static const char* TAG = "bb_touch";

static esp_lcd_touch_handle_t s_tp;
static esp_timer_handle_t s_timer;

/* 手势状态机 */
static int s_active;          /* 手指在屏上 */
static int64_t s_down_us;     /* 按下时刻 */
static int s_x0, s_y0;        /* 按下坐标 */
static int s_xl, s_yl;        /* 最新坐标 */
static int s_consumed;        /* 本次触摸已发过事件（长按），抬手不再判定 */

#define TOUCH_POLL_MS        20
#define SWIPE_MIN_PX         60  /* 410px 宽屏上 ~15% */
#define TAP_MAX_MOVE_PX      20
#define TAP_MAX_MS           400
#define LONG_PRESS_MS        600

static void emit(bb_nav_event_t ev, const char* name) {
  ESP_LOGI(TAG, "gesture=%s dx=%d dy=%d dur_ms=%lld", name, s_xl - s_x0, s_yl - s_y0,
           (long long)((esp_timer_get_time() - s_down_us) / 1000));
  bb_nav_input_inject(ev);
}

static void touch_poll_cb(void* arg) {
  (void)arg;
  if (esp_lcd_touch_read_data(s_tp) != ESP_OK) return;

  uint16_t x[1], y[1];
  uint8_t cnt = 0;
  bool pressed = esp_lcd_touch_get_coordinates(s_tp, x, y, NULL, &cnt, 1);

  if (pressed && cnt > 0) {
    if (!s_active) {
      s_active = 1;
      s_consumed = 0;
      s_down_us = esp_timer_get_time();
      s_x0 = s_xl = (int)x[0];
      s_y0 = s_yl = (int)y[0];
    } else {
      s_xl = (int)x[0];
      s_yl = (int)y[0];
      /* 长按（未位移）→ BACK；每次触摸只发一次 */
      const int moved = abs(s_xl - s_x0) > TAP_MAX_MOVE_PX || abs(s_yl - s_y0) > TAP_MAX_MOVE_PX;
      if (!s_consumed && !moved &&
          (esp_timer_get_time() - s_down_us) / 1000 >= LONG_PRESS_MS) {
        s_consumed = 1;
        emit(BB_NAV_EVENT_BACK, "long-press");
      }
    }
    return;
  }

  if (!s_active) return;
  /* 抬手：判定 tap / swipe */
  s_active = 0;
  if (s_consumed) return;
  const int dx = s_xl - s_x0;
  const int dy = s_yl - s_y0;
  const long long dur_ms = (esp_timer_get_time() - s_down_us) / 1000;
  if (abs(dy) >= SWIPE_MIN_PX && abs(dy) >= abs(dx)) {
    /* 内容跟手：上滑（dy<0）= 看下面 = 选中下移 */
    emit(dy < 0 ? BB_NAV_EVENT_DOWN : BB_NAV_EVENT_UP, dy < 0 ? "swipe-up" : "swipe-down");
  } else if (dx >= SWIPE_MIN_PX) {
    emit(BB_NAV_EVENT_BACK, "swipe-right");
  } else if (dx <= -SWIPE_MIN_PX) {
    /* 左滑暂不映射（留给未来快捷入口） */
  } else if (abs(dx) <= TAP_MAX_MOVE_PX && abs(dy) <= TAP_MAX_MOVE_PX && dur_ms <= TAP_MAX_MS) {
    emit(BB_NAV_EVENT_OK, "tap");
  }
}

esp_err_t bb_touch_input_init(void) {
  i2c_master_bus_handle_t bus = bb_audio_shared_i2c_bus();
  if (bus == NULL) {
    ESP_LOGW(TAG, "shared i2c bus not ready; touch disabled");
    return ESP_ERR_INVALID_STATE;
  }

  esp_lcd_panel_io_handle_t io = NULL;
  esp_lcd_panel_io_i2c_config_t io_cfg = ESP_LCD_TOUCH_IO_I2C_FT5x06_CONFIG();
  io_cfg.scl_speed_hz = 400000;
  ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_i2c(bus, &io_cfg, &io), TAG, "touch panel io");

  const esp_lcd_touch_config_t tp_cfg = {
      .x_max = BBCLAW_ST7789_WIDTH,
      .y_max = BBCLAW_ST7789_HEIGHT,
      .rst_gpio_num = BBCLAW_TOUCH_RST_GPIO,
      .int_gpio_num = BBCLAW_TOUCH_INT_GPIO,
      .levels = {
          .reset = 0,
          .interrupt = 0,
      },
      .flags = {
          .swap_xy = 0,
          .mirror_x = 0,
          .mirror_y = 0,
      },
  };
  ESP_RETURN_ON_ERROR(esp_lcd_touch_new_i2c_ft5x06(io, &tp_cfg, &s_tp), TAG, "ft5x06 new");

  const esp_timer_create_args_t targs = {
      .callback = touch_poll_cb,
      .name = "bb_touch_poll",
  };
  ESP_RETURN_ON_ERROR(esp_timer_create(&targs, &s_timer), TAG, "timer create");
  ESP_RETURN_ON_ERROR(esp_timer_start_periodic(s_timer, TOUCH_POLL_MS * 1000), TAG, "timer start");

  ESP_LOGI(TAG, "touch ready (ft5x06-compat, poll=%dms, tap=OK swipe=UP/DOWN long/right=BACK)",
           TOUCH_POLL_MS);
  return ESP_OK;
}

#else /* !BBCLAW_TOUCH_FT5X06_ENABLE */

esp_err_t bb_touch_input_init(void) { return ESP_OK; }

#endif
