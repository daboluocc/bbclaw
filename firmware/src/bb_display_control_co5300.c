/**
 * CO5300 AMOLED 亮度控制实现
 *
 * Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板
 * 通过 QSPI 面板指令设置亮度（0x51）
 */

#include "bb_display_control.h"
#include "bb_config.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"

#include <esp_log.h>
#include <esp_check.h>
#include "esp_lvgl_port.h"

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
/* Forwards from bb_lvgl_display.c */
extern esp_lcd_panel_handle_t bb_display_get_panel_handle(void);
extern esp_lcd_panel_io_handle_t bb_display_get_panel_io_handle(void);

esp_err_t bb_display_set_brightness_raw_impl(uint8_t value) {
  esp_lcd_panel_io_handle_t panel_io = bb_display_get_panel_io_handle();

  if (!panel_io) {
    ESP_LOGE(TAG, "Display panel IO not initialized");
    return ESP_ERR_INVALID_STATE;
  }

  /* 通过 QSPI 发送亮度指令。必须与 LVGL 刷屏互斥:同一 QSPI IO 上
   * tx_param 撞上在飞的 tx_color DMA 会打烂驱动状态(息屏管理随机崩的
   * 根因之一)。flush 全程在 LVGL 锁内完成(canvas 路径逐条带同步等 DMA),
   * 拿到锁即保证总线空闲。拿不到就跳过本步,渐变下一步补上。 */
  if (!lvgl_port_lock(100)) {
    return ESP_OK;
  }
  esp_err_t ret = esp_lcd_panel_io_tx_param(panel_io, CO5300_BRIGHTNESS_CMD, &value, 1);
  lvgl_port_unlock();

  if (ret == ESP_OK) {
    ESP_LOGD(TAG, "CO5300 brightness set to 0x%02x", value);
  } else {
    ESP_LOGE(TAG, "Failed to set brightness: %s", esp_err_to_name(ret));
  }

  return ret;
}
