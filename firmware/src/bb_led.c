#include "bb_led.h"

#include <stdbool.h>
#include <stdint.h>

#include "bb_config.h"
#include "bb_time.h"
#include "driver/gpio.h"
#include "driver/ledc.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char* TAG = "bb_led";

#define BB_LED_TASK_PERIOD_MS 30
#define BB_LED_LEDC_MODE LEDC_LOW_SPEED_MODE
#define BB_LED_LEDC_TIMER LEDC_TIMER_0
#define BB_LED_LEDC_DUTY_RES LEDC_TIMER_10_BIT
#define BB_LED_LEDC_FREQ_HZ 4000
#define BB_LED_LEDC_MAX_DUTY ((1U << 10) - 1U)

typedef struct {
  bb_led_status_t base_status;
  bb_led_status_t overlay_status;
  int64_t overlay_start_ms;
  int64_t overlay_deadline_ms;
} led_state_t;

static portMUX_TYPE s_lock = portMUX_INITIALIZER_UNLOCKED;
static led_state_t s_state = {
    .base_status = BB_LED_IDLE,
    .overlay_status = BB_LED_IDLE,
};
static bool s_overlay_active;
static bool s_ready;
static bool s_boot_anim_active;
static int64_t s_boot_anim_start_ms;
static uint32_t s_led_on_duty;
static uint32_t s_led_off_duty;

#if BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE && BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS > 0
#define BB_LED_BOOT_TOTAL_MS (3U * BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS * BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS)
#else
#define BB_LED_BOOT_TOTAL_MS 0U
#endif

static uint32_t clamp_brightness_pct(void) {
  if (BBCLAW_STATUS_LED_BRIGHTNESS_PCT <= 0) {
    return 0U;
  }
  if (BBCLAW_STATUS_LED_BRIGHTNESS_PCT >= 100) {
    return 100U;
  }
  return (uint32_t)BBCLAW_STATUS_LED_BRIGHTNESS_PCT;
}

static void ledc_apply(ledc_channel_t channel, int on) {
  uint32_t duty = on ? s_led_on_duty : s_led_off_duty;
  (void)ledc_set_duty(BB_LED_LEDC_MODE, channel, duty);
  (void)ledc_update_duty(BB_LED_LEDC_MODE, channel);
}

#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
/** 共阴 RGB 模块：R/G/B 三线，高电平点亮（与 led-RGB.png 一致） */
static void led_set_rgb_bits(int r, int g, int b) {
  ledc_apply(LEDC_CHANNEL_0, r);
  ledc_apply(LEDC_CHANNEL_1, g);
  ledc_apply(LEDC_CHANNEL_2, b);
}
#else
static void led_set_ryg_bits(int r, int y, int g) {
  ledc_apply(LEDC_CHANNEL_0, r);
  ledc_apply(LEDC_CHANNEL_1, y);
  ledc_apply(LEDC_CHANNEL_2, g);
}
#endif

static void led_all_off(void) {
#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  led_set_rgb_bits(0, 0, 0);
#else
  led_set_ryg_bits(0, 0, 0);
#endif
}

static uint32_t overlay_duration_ms(bb_led_status_t status) {
  switch (status) {
    case BB_LED_REPLY:
      return 900;
    case BB_LED_SUCCESS:
      return 360;
    case BB_LED_ERROR:
      return 1080;
    default:
      return 0;
  }
}

static esp_err_t led_init_pwm_channels(void) {
  const uint32_t brightness_pct = clamp_brightness_pct();
  const uint32_t active_duty = (BB_LED_LEDC_MAX_DUTY * brightness_pct) / 100U;

  if (BBCLAW_STATUS_LED_GPIO_ON_LEVEL) {
    s_led_on_duty = active_duty;
    s_led_off_duty = 0U;
  } else {
    s_led_on_duty = BB_LED_LEDC_MAX_DUTY - active_duty;
    s_led_off_duty = BB_LED_LEDC_MAX_DUTY;
  }

  ledc_timer_config_t timer_cfg = {
      .speed_mode = BB_LED_LEDC_MODE,
      .duty_resolution = BB_LED_LEDC_DUTY_RES,
      .timer_num = BB_LED_LEDC_TIMER,
      .freq_hz = BB_LED_LEDC_FREQ_HZ,
      .clk_cfg = LEDC_AUTO_CLK,
  };
  esp_err_t err = ledc_timer_config(&timer_cfg);
  if (err != ESP_OK) {
    return err;
  }

#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  const int gpios[3] = {BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_RGB_G_GPIO, BBCLAW_STATUS_LED_RGB_B_GPIO};
#else
  const int gpios[3] = {BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_Y_GPIO, BBCLAW_STATUS_LED_G_GPIO};
#endif

  for (int i = 0; i < 3; ++i) {
    ledc_channel_config_t ch_cfg = {
        .gpio_num = gpios[i],
        .speed_mode = BB_LED_LEDC_MODE,
        .channel = (ledc_channel_t)i,
        .intr_type = LEDC_INTR_DISABLE,
        .timer_sel = BB_LED_LEDC_TIMER,
        .duty = s_led_off_duty,
        .hpoint = 0,
        .sleep_mode = LEDC_SLEEP_MODE_NO_ALIVE_NO_PD,
    };
    err = ledc_channel_config(&ch_cfg);
    if (err != ESP_OK) {
      return err;
    }
  }
  return ESP_OK;
}

static void render_boot_marquee(uint32_t elapsed_ms) {
  uint32_t step = (uint32_t)BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS;
  if (step == 0U) {
    step = 1U;
  }
  uint32_t idx = (elapsed_ms / step) % 3U;
#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  if (idx == 0U) {
    led_set_rgb_bits(1, 0, 0);
  } else if (idx == 1U) {
    led_set_rgb_bits(0, 1, 0);
  } else {
    led_set_rgb_bits(0, 0, 1);
  }
#else
  if (idx == 0U) {
    led_set_ryg_bits(1, 0, 0);
  } else if (idx == 1U) {
    led_set_ryg_bits(0, 1, 0);
  } else {
    led_set_ryg_bits(0, 0, 1);
  }
#endif
}

static void render_led(bb_led_status_t status, bool overlay_active, uint32_t elapsed_ms) {
  uint32_t phase;

#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  switch (status) {
    case BB_LED_IDLE:
      led_set_rgb_bits(0, 1, 0);
      break;
    case BB_LED_RECORDING:
      led_set_rgb_bits(1, 0, 0);
      break;
    case BB_LED_PROCESSING:
      phase = (elapsed_ms / 250U) % 2U;
      if (phase == 0U) {
        led_set_rgb_bits(1, 1, 0);
      } else {
        led_set_rgb_bits(0, 0, 0);
      }
      break;
    case BB_LED_REPLY:
      phase = (elapsed_ms / 250U) % 2U;
      if (phase == 0U) {
        led_set_rgb_bits(0, 1, 0);
      } else {
        led_set_rgb_bits(0, 0, 0);
      }
      break;
    case BB_LED_NOTIFICATION:
      phase = (elapsed_ms / 200U) % 2U;
      if (phase == 0U) {
        led_set_rgb_bits(1, 0, 0);
      } else {
        led_set_rgb_bits(0, 0, 0);
      }
      break;
    case BB_LED_SUCCESS:
      if (overlay_active && elapsed_ms < 200U) {
        led_set_rgb_bits(0, 1, 0);
      } else {
        led_set_rgb_bits(0, 0, 0);
      }
      break;
    case BB_LED_ERROR:
      phase = (elapsed_ms / 250U) % 2U;
      if (phase == 0U) {
        led_set_rgb_bits(1, 0, 0);
      } else {
        led_set_rgb_bits(0, 0, 0);
      }
      break;
    default:
      led_set_rgb_bits(0, 0, 0);
      break;
  }
#else
  switch (status) {
    case BB_LED_IDLE: {
      led_set_ryg_bits(0, 0, 1);
      break;
    }
    case BB_LED_RECORDING: {
      led_set_ryg_bits(1, 0, 0);
      break;
    }
    case BB_LED_PROCESSING: {
      phase = (elapsed_ms / 250U) % 2U;
      if (phase == 0U) {
        led_set_ryg_bits(0, 1, 0);
      } else {
        led_set_ryg_bits(0, 0, 0);
      }
      break;
    }
    case BB_LED_REPLY: {
      phase = (elapsed_ms / 250U) % 2U;
      if (phase == 0U) {
        led_set_ryg_bits(0, 0, 1);
      } else {
        led_set_ryg_bits(0, 0, 0);
      }
      break;
    }
    case BB_LED_NOTIFICATION: {
      phase = (elapsed_ms / 200U) % 2U;
      if (phase == 0U) {
        led_set_ryg_bits(1, 0, 0);
      } else {
        led_set_ryg_bits(0, 0, 0);
      }
      break;
    }
    case BB_LED_SUCCESS: {
      if (overlay_active && elapsed_ms < 200U) {
        led_set_ryg_bits(0, 0, 1);
      } else {
        led_set_ryg_bits(0, 0, 0);
      }
      break;
    }
    case BB_LED_ERROR: {
      phase = (elapsed_ms / 250U) % 2U;
      if (phase == 0U) {
        led_set_ryg_bits(1, 0, 0);
      } else {
        led_set_ryg_bits(0, 0, 0);
      }
      break;
    }
    default:
      led_set_ryg_bits(0, 0, 0);
      break;
  }
#endif
}

static void led_task(void* arg) {
  (void)arg;
  while (1) {
    bb_led_status_t active_status = BB_LED_IDLE;
    bool overlay_active = false;
    int64_t overlay_start_ms = 0;
    int64_t now_ms = bb_now_ms();

    portENTER_CRITICAL(&s_lock);
    if (s_overlay_active && now_ms >= s_state.overlay_deadline_ms) {
      s_overlay_active = false;
    }
    overlay_active = s_overlay_active;
    active_status = overlay_active ? s_state.overlay_status : s_state.base_status;
    overlay_start_ms = s_state.overlay_start_ms;
    portEXIT_CRITICAL(&s_lock);

    if (s_boot_anim_active) {
      uint32_t boot_elapsed = (uint32_t)(now_ms - s_boot_anim_start_ms);
      if (boot_elapsed >= BB_LED_BOOT_TOTAL_MS) {
        s_boot_anim_active = false;
      } else {
        render_boot_marquee(boot_elapsed);
        vTaskDelay(pdMS_TO_TICKS(BB_LED_TASK_PERIOD_MS));
        continue;
      }
    }

    uint32_t elapsed_ms = overlay_active ? (uint32_t)(now_ms - overlay_start_ms) : (uint32_t)now_ms;
    render_led(active_status, overlay_active, elapsed_ms);
    vTaskDelay(pdMS_TO_TICKS(BB_LED_TASK_PERIOD_MS));
  }
}

esp_err_t bb_led_init(void) {
  if (!BBCLAW_STATUS_LED_ENABLE) {
    ESP_LOGI(TAG, "status led disabled by config");
    return ESP_OK;
  }
  if (s_ready) {
    return ESP_OK;
  }

#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  if (BBCLAW_STATUS_LED_R_GPIO < 0 || BBCLAW_STATUS_LED_RGB_G_GPIO < 0 || BBCLAW_STATUS_LED_RGB_B_GPIO < 0) {
    ESP_LOGW(TAG, "rgb module enabled but gpio not fully configured");
    return ESP_ERR_INVALID_ARG;
  }
  if (BBCLAW_STATUS_LED_R_GPIO == BBCLAW_STATUS_LED_RGB_G_GPIO ||
      BBCLAW_STATUS_LED_R_GPIO == BBCLAW_STATUS_LED_RGB_B_GPIO ||
      BBCLAW_STATUS_LED_RGB_G_GPIO == BBCLAW_STATUS_LED_RGB_B_GPIO) {
    ESP_LOGW(TAG, "rgb module gpio must be unique r=%d g=%d b=%d", BBCLAW_STATUS_LED_R_GPIO,
             BBCLAW_STATUS_LED_RGB_G_GPIO, BBCLAW_STATUS_LED_RGB_B_GPIO);
    return ESP_ERR_INVALID_ARG;
  }
  uint64_t mask = (1ULL << (unsigned)BBCLAW_STATUS_LED_R_GPIO) | (1ULL << (unsigned)BBCLAW_STATUS_LED_RGB_G_GPIO) |
                  (1ULL << (unsigned)BBCLAW_STATUS_LED_RGB_B_GPIO);
#else
  if (BBCLAW_STATUS_LED_R_GPIO < 0 || BBCLAW_STATUS_LED_Y_GPIO < 0 || BBCLAW_STATUS_LED_G_GPIO < 0) {
    ESP_LOGW(TAG, "status led enabled but gpio not fully configured");
    return ESP_ERR_INVALID_ARG;
  }
  if (BBCLAW_STATUS_LED_R_GPIO == BBCLAW_STATUS_LED_Y_GPIO || BBCLAW_STATUS_LED_R_GPIO == BBCLAW_STATUS_LED_G_GPIO ||
      BBCLAW_STATUS_LED_Y_GPIO == BBCLAW_STATUS_LED_G_GPIO) {
    ESP_LOGW(TAG, "status led gpio must be unique r=%d y=%d g=%d", BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_Y_GPIO,
             BBCLAW_STATUS_LED_G_GPIO);
    return ESP_ERR_INVALID_ARG;
  }
  uint64_t mask = (1ULL << (unsigned)BBCLAW_STATUS_LED_R_GPIO) | (1ULL << (unsigned)BBCLAW_STATUS_LED_Y_GPIO) |
                  (1ULL << (unsigned)BBCLAW_STATUS_LED_G_GPIO);
#endif

  gpio_config_t io = {
      .pin_bit_mask = mask,
      .mode = GPIO_MODE_OUTPUT,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  esp_err_t err = gpio_config(&io);
  if (err != ESP_OK) {
    return err;
  }
  err = led_init_pwm_channels();
  if (err != ESP_OK) {
    return err;
  }
  led_all_off();

#if BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE && BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS > 0
  s_boot_anim_start_ms = bb_now_ms();
  s_boot_anim_active = true;
  ESP_LOGI(TAG, "boot marquee total_ms=%u step_ms=%d loops=%d", (unsigned)BB_LED_BOOT_TOTAL_MS,
           BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS, BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS);
#else
  s_boot_anim_active = false;
#endif

  BaseType_t ok = xTaskCreate(led_task, "bb_led_task", 3072, NULL, 3, NULL);
  if (ok != pdPASS) {
    s_boot_anim_active = false;
    return ESP_ERR_NO_MEM;
  }

  s_ready = true;
#if BBCLAW_STATUS_LED_KIND_RGB_MODULE
  ESP_LOGI(TAG, "status led rgb module r=%d g=%d b=%d gpio_on_level=%d brightness_pct=%d pwm=%uHz/%ubit (yellow=R+G)",
           BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_RGB_G_GPIO, BBCLAW_STATUS_LED_RGB_B_GPIO,
           BBCLAW_STATUS_LED_GPIO_ON_LEVEL, BBCLAW_STATUS_LED_BRIGHTNESS_PCT, BB_LED_LEDC_FREQ_HZ, 10U);
#else
  ESP_LOGI(TAG, "status led ryg r=%d y=%d g=%d gpio_on_level=%d brightness_pct=%d pwm=%uHz/%ubit", BBCLAW_STATUS_LED_R_GPIO,
           BBCLAW_STATUS_LED_Y_GPIO, BBCLAW_STATUS_LED_G_GPIO, BBCLAW_STATUS_LED_GPIO_ON_LEVEL,
           BBCLAW_STATUS_LED_BRIGHTNESS_PCT, BB_LED_LEDC_FREQ_HZ, 10U);
#endif
  return ESP_OK;
}

esp_err_t bb_led_set_status(bb_led_status_t status) {
  if (!BBCLAW_STATUS_LED_ENABLE) {
    return ESP_OK;
  }
  if (!s_ready) {
    return ESP_ERR_INVALID_STATE;
  }

  uint32_t duration_ms = overlay_duration_ms(status);
  int64_t now_ms = bb_now_ms();

  portENTER_CRITICAL(&s_lock);
  if (duration_ms == 0U) {
    s_state.base_status = status;
  } else {
    s_state.overlay_status = status;
    s_state.overlay_start_ms = now_ms;
    s_state.overlay_deadline_ms = now_ms + duration_ms;
    s_overlay_active = true;
  }
  portEXIT_CRITICAL(&s_lock);

  return ESP_OK;
}
