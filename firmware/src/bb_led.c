#include "bb_led.h"

#include <stdbool.h>
#include <stdint.h>

#include "bb_config.h"
#include "bb_time.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char* TAG = "bb_led";

#define BB_LED_TASK_PERIOD_MS 30

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

#if BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE && BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS > 0
#define BB_LED_BOOT_TOTAL_MS (3U * BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS * BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS)
#else
#define BB_LED_BOOT_TOTAL_MS 0U
#endif

static inline int led_on_level(void) {
  return BBCLAW_STATUS_LED_GPIO_ON_LEVEL ? 1 : 0;
}

static inline int led_off_level(void) {
  return BBCLAW_STATUS_LED_GPIO_ON_LEVEL ? 0 : 1;
}

static void led_set_ryg_bits(int r, int y, int g) {
  int on = led_on_level();
  int off = led_off_level();
  (void)gpio_set_level(BBCLAW_STATUS_LED_R_GPIO, r ? on : off);
  (void)gpio_set_level(BBCLAW_STATUS_LED_Y_GPIO, y ? on : off);
  (void)gpio_set_level(BBCLAW_STATUS_LED_G_GPIO, g ? on : off);
}

/** 共阴 RGB 模块：R/G/B 三线，高电平点亮（与 led-RGB.png 一致） */
static void led_set_rgb_bits(int r, int g, int b) {
  int on = led_on_level();
  int off = led_off_level();
  (void)gpio_set_level(BBCLAW_STATUS_LED_R_GPIO, r ? on : off);
  (void)gpio_set_level(BBCLAW_STATUS_LED_RGB_G_GPIO, g ? on : off);
  (void)gpio_set_level(BBCLAW_STATUS_LED_RGB_B_GPIO, b ? on : off);
}

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
  ESP_LOGI(TAG, "status led rgb module r=%d g=%d b=%d gpio_on_level=%d (yellow=R+G)",
           BBCLAW_STATUS_LED_R_GPIO, BBCLAW_STATUS_LED_RGB_G_GPIO, BBCLAW_STATUS_LED_RGB_B_GPIO,
           BBCLAW_STATUS_LED_GPIO_ON_LEVEL);
#else
  ESP_LOGI(TAG, "status led ryg r=%d y=%d g=%d gpio_on_level=%d (digital)", BBCLAW_STATUS_LED_R_GPIO,
           BBCLAW_STATUS_LED_Y_GPIO, BBCLAW_STATUS_LED_G_GPIO, BBCLAW_STATUS_LED_GPIO_ON_LEVEL);
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
