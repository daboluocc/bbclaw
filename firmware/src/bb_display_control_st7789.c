/**
 * ST7789 / I80-LCD 息屏 + 亮度控制实现（bbclaw 生产板等 SPI/I80 屏）
 *
 * 与 CO5300(AMOLED,自发光,靠 DISPOFF+SLPIN 熄像素)不同:这类 TFT-LCD 的功耗大头
 * 是**背光**,息屏 = 关背光 GPIO(近全灭功耗) + 面板 DISPOFF(0x28,防残影/再省一点)。
 * 背光目前是纯 GPIO 开关(非 PWM),故亮度只做 on/off:raw=0 灭,raw>0 亮;真·调光
 * 需给 BL 脚接 LEDC PWM,列为后续增强。ADR-047 bbclaw 段。
 *
 * 板级门控:QSPI(CO5300)走 bb_display_control_co5300.c,本文件覆盖其余(SPI/I80)。
 * 两文件互斥定义 *_impl 符号,避免重复符号。
 */

#include "bb_display_control.h"
#include "bb_config.h"

#if !BBCLAW_DISPLAY_BUS_QSPI

#include "esp_lcd_panel_ops.h"
#include "driver/gpio.h"

#include <esp_log.h>
#include "esp_lvgl_port.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char* TAG = "display_st7789";

/* Forwards from bb_lvgl_display.c */
extern esp_lcd_panel_handle_t bb_display_get_panel_handle(void);

/* 背光开关(纯 GPIO)。无 BL 脚的板(如 atk I80,BL=-1)整段跳过。 */
static void st7789_backlight(int on) {
#if BBCLAW_ST7789_BL_GPIO >= 0
  gpio_set_level(BBCLAW_ST7789_BL_GPIO, on ? BBCLAW_ST7789_BL_ACTIVE_LEVEL : !BBCLAW_ST7789_BL_ACTIVE_LEVEL);
#else
  (void)on;
#endif
}

esp_err_t bb_display_set_brightness_raw_impl(uint8_t value) {
  /* LCD 无寄存器亮度:背光 GPIO on/off。raw=0 灭,>0 亮。 */
  st7789_backlight(value > 0 ? 1 : 0);
  ESP_LOGD(TAG, "ST7789 backlight %s (raw=0x%02x)", value > 0 ? "on" : "off", value);
  return ESP_OK;
}

/* 息屏/唤醒。开:先 DISPON 再开背光(避免先亮背光看到残帧闪);
 * 关:先 DISPOFF 停面板扫描,再灭背光。面板命令走 SPI/I80 总线,与 LVGL 刷屏
 * 互斥(同 co5300 的教训),拿 lvgl_port_lock 保证总线空闲。 */
esp_err_t bb_display_set_panel_on_impl(int on) {
  esp_lcd_panel_handle_t panel = bb_display_get_panel_handle();
  if (on) {
    if (panel && lvgl_port_lock(200)) {
      esp_lcd_panel_disp_on_off(panel, true); /* DISPON 0x29 */
      lvgl_port_unlock();
    }
    st7789_backlight(1);
    ESP_LOGI(TAG, "ST7789 panel ON (DISPON + backlight on)");
  } else {
    st7789_backlight(0);
    if (panel && lvgl_port_lock(200)) {
      esp_lcd_panel_disp_on_off(panel, false); /* DISPOFF 0x28 */
      lvgl_port_unlock();
    }
    ESP_LOGI(TAG, "ST7789 panel OFF (backlight off + DISPOFF)");
  }
  return ESP_OK;
}

#endif /* !BBCLAW_DISPLAY_BUS_QSPI */
