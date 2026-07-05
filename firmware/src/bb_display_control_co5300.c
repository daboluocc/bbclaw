/**
 * CO5300 AMOLED 亮度控制实现
 *
 * Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板
 * 通过 QSPI 面板指令设置亮度（0x51）
 */

#include "bb_display_control.h"
#include "bb_board_config.h"
#include "bb_lvgl_display.h"

#include <esp_log.h>
#include <esp_check.h>

static const char* TAG = "display_co5300";

/* ── CO5300 显示指令 ── */
#define CO5300_BRIGHTNESS_CMD  0x51  /* Write Display Brightness */

/**
 * 设置 CO5300 AMOLED 屏幕亮度。
 *
 * 通过 QSPI 接口发送 0x51 指令 + 亮度字节。
 * 由 bb_display_control.c 调用。
 *
 * @param value 亮度值 (0-255)
 * @return ESP_OK 成功；ESP_ERR_NOT_SUPPORTED 硬件不支持
 */
esp_err_t bb_display_set_brightness_raw_impl(uint8_t value) {
  /* 获取面板句柄（由 bb_lvgl_display.c 提供） */
  extern esp_lcd_panel_handle_t bb_display_get_panel_handle(void);
  esp_lcd_panel_handle_t panel = bb_display_get_panel_handle();

  if (!panel) {
    ESP_LOGE(TAG, "Display panel not initialized");
    return ESP_ERR_INVALID_STATE;
  }

  /* 通过 QSPI 发送亮度指令 */
  esp_err_t ret = esp_lcd_panel_io_tx_param(
    bb_display_get_panel_io_handle(),  /* 需要从 bb_lvgl_display 导出 panel_io */
    CO5300_BRIGHTNESS_CMD,
    &value,
    1
  );

  if (ret == ESP_OK) {
    ESP_LOGD(TAG, "CO5300 brightness set to 0x%02x", value);
  } else {
    ESP_LOGE(TAG, "Failed to set brightness: %s", esp_err_to_name(ret));
  }

  return ret;
}
