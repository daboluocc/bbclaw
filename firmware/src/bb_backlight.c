/**
 * bb_backlight.c — ST7789 背光统一开关。见 bb_backlight.h（实战派 PWM 结论）。
 */
#include "bb_backlight.h"
#include "bb_config.h"

#if BBCLAW_ST7789_BL_GPIO >= 0

#if BBCLAW_ST7789_BL_PWM

#include <driver/ledc.h>

static int s_init;

static esp_err_t init_once(void) {
  if (s_init) return ESP_OK;
  ledc_timer_config_t tcfg = {
      .speed_mode = LEDC_LOW_SPEED_MODE,
      .duty_resolution = LEDC_TIMER_10_BIT,
      .timer_num = LEDC_TIMER_1, /* bb_led 用 TIMER_0/CH0-2，避开 */
      .freq_hz = BBCLAW_ST7789_BL_PWM_FREQ_HZ,
      .clk_cfg = LEDC_AUTO_CLK,
  };
  esp_err_t err = ledc_timer_config(&tcfg);
  if (err != ESP_OK) return err;
  ledc_channel_config_t ccfg = {
      .gpio_num = BBCLAW_ST7789_BL_GPIO,
      .speed_mode = LEDC_LOW_SPEED_MODE,
      .channel = LEDC_CHANNEL_3,
      .timer_sel = LEDC_TIMER_1,
      .duty = 0,
      .hpoint = 0,
  };
  err = ledc_channel_config(&ccfg);
  if (err != ESP_OK) return err;
  s_init = 1;
  return ESP_OK;
}

esp_err_t bb_backlight_set(int on) {
  esp_err_t err = init_once();
  if (err != ESP_OK) return err;
  err = ledc_set_duty(LEDC_LOW_SPEED_MODE, LEDC_CHANNEL_3, on ? BBCLAW_ST7789_BL_PWM_ON_DUTY : 0);
  if (err != ESP_OK) return err;
  return ledc_update_duty(LEDC_LOW_SPEED_MODE, LEDC_CHANNEL_3);
}

#else /* !BBCLAW_ST7789_BL_PWM: 传统 GPIO 电平背光 */

#include <driver/gpio.h>

static int s_init;

esp_err_t bb_backlight_set(int on) {
  if (!s_init) {
    gpio_config_t io_conf = {
        .pin_bit_mask = 1ULL << BBCLAW_ST7789_BL_GPIO,
        .mode = GPIO_MODE_OUTPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    esp_err_t err = gpio_config(&io_conf);
    if (err != ESP_OK) return err;
    s_init = 1;
  }
  return gpio_set_level(BBCLAW_ST7789_BL_GPIO,
                        on ? BBCLAW_ST7789_BL_ACTIVE_LEVEL : !BBCLAW_ST7789_BL_ACTIVE_LEVEL);
}

#endif

#else /* 无背光脚 */

esp_err_t bb_backlight_set(int on) {
  (void)on;
  return ESP_OK;
}

#endif
