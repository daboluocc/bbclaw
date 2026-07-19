/**
 * bb_touch_input.c — FT5x06 兼容触控（手表 FT3168）→ LVGL 原生指针 indev。
 *
 * 阶段演进（ADR-040 §UI.5）：
 *   v1 手势层：tap/swipe/长按 → bb_nav_input_inject（按键语义模拟）。
 *   v2（现行）：注册 esp_lvgl_port 指针 indev——跟手滚动 / 行点按 / 拖动
 *   全部 LVGL 原生；屏上 PTT 钮走 LVGL PRESSED/RELEASED 事件（bb_lvgl_display）；
 *   右滑 BACK 保留为屏幕级 LVGL 手势。列表行的点按由各 UI 模块挂
 *   LV_EVENT_CLICKED。物理 BOOT 键与 devmon 注入通路不受影响。
 */
#include "bb_touch_input.h"
#include "bb_power_mgmt.h"

#include "bb_config.h"

#if BBCLAW_TOUCH_FT5X06_ENABLE

#include "bb_audio.h"
#include "bb_display.h"
#include "bb_nav_input.h"
#include "bb_state.h"
#include "driver/i2c_master.h"
#include "esp_check.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_touch_ft5x06.h"
#include "esp_log.h"
#include "esp_lvgl_port.h"
#include "lvgl.h"

static const char* TAG = "bb_touch";

static esp_lcd_touch_handle_t s_tp;

/* ADR-046: 触摸活动上报(息屏管理) */
static void touch_activity_cb(lv_event_t* e) {
  (void)e;
  bb_power_mgmt_on_user_activity();
}

/* 屏幕级手势：右滑 = BACK（返回/退出，与旧手势层语义一致） */
static void screen_gesture_cb(lv_event_t* e) {
  (void)e;
  const lv_dir_t dir = lv_indev_get_gesture_dir(lv_indev_active());
  if (dir == LV_DIR_RIGHT) {
    ESP_LOGI(TAG, "gesture=swipe-right -> BACK");
    bb_nav_input_inject(BB_NAV_EVENT_BACK);
  } else if (dir == LV_DIR_LEFT) {
    /* 左滑 → LEFT：竖屏聊天态映射为「打开设置」（回复中也可进，TTS 不断），
     * 避免用右滑(BACK)进设置——busy 时 BACK 语义是取消回合 */
    ESP_LOGI(TAG, "gesture=swipe-left -> LEFT");
    bb_nav_input_inject(BB_NAV_EVENT_LEFT);
  }
}

/* 空白单点 = BACK(统一语义,见 design/firmware_touch_interaction.md)。
 * 仅设置态生效:聊天页是主页无处可退,且 busy 时 BACK=取消回合,误触不可接受。
 * target==屏幕本身 才算"空白"(命中可点控件时事件不会落到屏幕上)。 */
static void screen_blank_tap_cb(lv_event_t* e) {
  if (lv_event_get_target(e) != lv_screen_active()) return;
  const bb_state_t st = bb_state_get();
  if (st.page != BB_PAGE_SETTINGS) return;
  ESP_LOGI(TAG, "blank-tap -> BACK (settings)");
  bb_nav_input_inject(BB_NAV_EVENT_BACK);
}

esp_err_t bb_touch_input_init(void) {
  i2c_master_bus_handle_t bus = bb_audio_shared_i2c_bus();
  if (bus == NULL) {
    ESP_LOGW(TAG, "shared i2c bus not ready; touch disabled");
    return ESP_ERR_INVALID_STATE;
  }
  lv_display_t* disp = (lv_display_t*)bb_display_get_lv_display();
  if (disp == NULL) {
    ESP_LOGW(TAG, "lv display not ready; touch disabled");
    return ESP_ERR_INVALID_STATE;
  }

  esp_lcd_panel_io_handle_t io = NULL;
  esp_lcd_panel_io_i2c_config_t io_cfg = ESP_LCD_TOUCH_IO_I2C_FT5x06_CONFIG();
  io_cfg.scl_speed_hz = 400000;
  ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_i2c(bus, &io_cfg, &io), TAG, "touch panel io");

  /* x_max/y_max 是触摸控制器的原生量程(竖屏面板坐标);横屏板 swap_xy=1 时
   * 原生宽=显示高、原生高=显示宽,变换后落到显示坐标系。 */
  const esp_lcd_touch_config_t tp_cfg = {
#if BBCLAW_TOUCH_SWAP_XY
      .x_max = BBCLAW_ST7789_HEIGHT,
      .y_max = BBCLAW_ST7789_WIDTH,
#else
      .x_max = BBCLAW_ST7789_WIDTH,
      .y_max = BBCLAW_ST7789_HEIGHT,
#endif
      .rst_gpio_num = BBCLAW_TOUCH_RST_GPIO,
      .int_gpio_num = BBCLAW_TOUCH_INT_GPIO,
      .levels = {
          .reset = 0,
          .interrupt = 0,
      },
      .flags = {
          .swap_xy = BBCLAW_TOUCH_SWAP_XY,
          .mirror_x = BBCLAW_TOUCH_MIRROR_X,
          .mirror_y = BBCLAW_TOUCH_MIRROR_Y,
      },
  };
  ESP_RETURN_ON_ERROR(esp_lcd_touch_new_i2c_ft5x06(io, &tp_cfg, &s_tp), TAG, "ft5x06 new");

  const lvgl_port_touch_cfg_t touch_cfg = {
      .disp = disp,
      .handle = s_tp,
  };
  lv_indev_t* indev = lvgl_port_add_touch(&touch_cfg);
  if (indev == NULL) {
    ESP_LOGE(TAG, "lvgl_port_add_touch failed");
    return ESP_FAIL;
  }
  /* ADR-046: 触摸按下=用户活动(息屏计时复位/亮屏)。LVGL 任务上下文,
   * 只置旗标(转换由 stream_task tick 执行),安全。 */
  lv_indev_add_event_cb(indev, touch_activity_cb, LV_EVENT_PRESSED, NULL);

  if (lvgl_port_lock(1000)) {
    lv_obj_add_event_cb(lv_screen_active(), screen_gesture_cb, LV_EVENT_GESTURE, NULL);
    lv_obj_add_event_cb(lv_screen_active(), screen_blank_tap_cb, LV_EVENT_CLICKED, NULL);
    lvgl_port_unlock();
  }

  ESP_LOGI(TAG, "touch ready (native lvgl indev; swipe-right=BACK, blank-tap=BACK in settings)");
  return ESP_OK;
}

#else /* !BBCLAW_TOUCH_FT5X06_ENABLE */

esp_err_t bb_touch_input_init(void) { return ESP_OK; }

#endif
