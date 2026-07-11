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
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char* TAG = "display_co5300";

/* ── CO5300 显示指令 ── */
#define CO5300_BRIGHTNESS_CMD  0x51  /* Write Display Brightness */

/* QSPI 命令编帧:SH8601/CO5300 走 QSPI 时命令必须编成 (0x02<<24)|(cmd<<8)
 * (LCD_OPCODE_WRITE_CMD=0x02)。init 命令经驱动内部 tx_param 已做此编帧;运行期
 * 直接调 esp_lcd_panel_io_tx_param 必须自己编,否则发出的是裸命令、面板忽略
 * (事务仍返回 OK)——这是亮度/息屏命令长期物理不生效的根因。 */
#define CO5300_QSPI_CMD(c) (((uint32_t)0x02u << 24) | (((uint32_t)(c) & 0xffu) << 8))

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
  esp_err_t ret = esp_lcd_panel_io_tx_param(panel_io, CO5300_QSPI_CMD(CO5300_BRIGHTNESS_CMD), &value, 1);
  lvgl_port_unlock();

  if (ret == ESP_OK) {
    ESP_LOGD(TAG, "CO5300 brightness set to 0x%02x", value);
  } else {
    ESP_LOGE(TAG, "Failed to set brightness: %s", esp_err_to_name(ret));
  }

  return ret;
}

/* 面板显示开关(息屏用)。实测 0x51 写亮度=0 熄不灭这块 CO5300 AMOLED,单发 0x28
 * DISPOFF 也不够——必须 DISPOFF(0x28)+SLPIN(0x10)真正下电,像素才熄灭(近零功耗)。
 * 唤醒需 SLPOUT(0x11)退睡眠 + ~120ms 稳定 + DISPON(0x29)。序列参考 Arduino_GFX
 * Arduino_CO5300 driver 的 displayOn/displayOff。命令与 LVGL 刷屏互斥(共用 QSPI)。 */
#define CO5300_C_SLPIN   0x10
#define CO5300_C_SLPOUT  0x11
#define CO5300_C_DISPOFF 0x28
#define CO5300_C_DISPON  0x29
esp_err_t bb_display_set_panel_on_impl(int on) {
  esp_lcd_panel_io_handle_t panel_io = bb_display_get_panel_io_handle();
  if (!panel_io) {
    return ESP_ERR_INVALID_STATE;
  }
  if (on) {
    /* 退睡眠(SLPOUT)后面板需 ~120ms 稳定,期间不持 LVGL 锁 */
    if (lvgl_port_lock(200)) {
      esp_lcd_panel_io_tx_param(panel_io, CO5300_QSPI_CMD(CO5300_C_SLPOUT), NULL, 0);
      lvgl_port_unlock();
    }
    vTaskDelay(pdMS_TO_TICKS(120));
    if (lvgl_port_lock(200)) {
      esp_lcd_panel_io_tx_param(panel_io, CO5300_QSPI_CMD(CO5300_C_DISPON), NULL, 0);
      lvgl_port_unlock();
    }
    ESP_LOGI(TAG, "CO5300 panel ON (SLPOUT+DISPON)");
  } else {
    if (lvgl_port_lock(200)) {
      esp_lcd_panel_io_tx_param(panel_io, CO5300_QSPI_CMD(CO5300_C_DISPOFF), NULL, 0);
      esp_lcd_panel_io_tx_param(panel_io, CO5300_QSPI_CMD(CO5300_C_SLPIN), NULL, 0);
      lvgl_port_unlock();
    }
    ESP_LOGI(TAG, "CO5300 panel OFF (DISPOFF+SLPIN)");
  }
  return ESP_OK;
}
