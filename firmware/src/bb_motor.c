#include "bb_motor.h"

#include <stdint.h>

#include "bb_config.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"

static const char* TAG = "bb_motor";

typedef struct {
  uint16_t on_ms;
  uint16_t off_ms;
  uint8_t repeat;
} motor_event_t;

static QueueHandle_t s_queue;
static int s_ready;

static void motor_set_level(int on) {
  int level = on ? BBCLAW_MOTOR_ACTIVE_LEVEL : (BBCLAW_MOTOR_ACTIVE_LEVEL ? 0 : 1);
  ESP_LOGD(TAG, "motor gpio=%d level=%d", BBCLAW_MOTOR_GPIO, level);
  (void)gpio_set_level(BBCLAW_MOTOR_GPIO, level);
}

static void motor_task(void* arg) {
  (void)arg;
  motor_event_t evt = {0};
  for (;;) {
    if (xQueueReceive(s_queue, &evt, portMAX_DELAY) != pdTRUE) {
      continue;
    }
    for (uint8_t i = 0; i < evt.repeat; ++i) {
      motor_set_level(1);
      vTaskDelay(pdMS_TO_TICKS(evt.on_ms));
      motor_set_level(0);
      if (i + 1U < evt.repeat && evt.off_ms > 0U) {
        vTaskDelay(pdMS_TO_TICKS(evt.off_ms));
      }
    }
  }
}

esp_err_t bb_motor_init(void) {
  if (!BBCLAW_MOTOR_ENABLE) {
    ESP_LOGI(TAG, "motor disabled by config");
    return ESP_OK;
  }
  if (s_ready) {
    return ESP_OK;
  }
  if (BBCLAW_MOTOR_GPIO < 0) {
    ESP_LOGW(TAG, "motor enabled but gpio not configured");
    return ESP_ERR_INVALID_ARG;
  }
  gpio_config_t cfg = {
      .pin_bit_mask = 1ULL << BBCLAW_MOTOR_GPIO,
      .mode = GPIO_MODE_OUTPUT,
      .pull_down_en = GPIO_PULLDOWN_DISABLE,
      .pull_up_en = GPIO_PULLUP_DISABLE,
      .intr_type = GPIO_INTR_DISABLE,
  };
  esp_err_t err = gpio_config(&cfg);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "motor gpio config failed err=%s", esp_err_to_name(err));
    return err;
  }
  motor_set_level(0);

  s_queue = xQueueCreate(8, sizeof(motor_event_t));
  if (s_queue == NULL) {
    return ESP_ERR_NO_MEM;
  }
  BaseType_t ok = xTaskCreate(motor_task, "bb_motor_task", 2048, NULL, 4, NULL);
  if (ok != pdPASS) {
    vQueueDelete(s_queue);
    s_queue = NULL;
    return ESP_ERR_NO_MEM;
  }
  s_ready = 1;
  ESP_LOGI(TAG, "motor ready gpio=%d active_level=%d", BBCLAW_MOTOR_GPIO, BBCLAW_MOTOR_ACTIVE_LEVEL);

  /* boot self-test: direct GPIO pulse bypassing task queue */
  ESP_LOGW(TAG, "motor self-test: gpio=%d HIGH for 500ms", BBCLAW_MOTOR_GPIO);
  gpio_set_level(BBCLAW_MOTOR_GPIO, BBCLAW_MOTOR_ACTIVE_LEVEL);
  vTaskDelay(pdMS_TO_TICKS(500));
  gpio_set_level(BBCLAW_MOTOR_GPIO, BBCLAW_MOTOR_ACTIVE_LEVEL ? 0 : 1);
  ESP_LOGW(TAG, "motor self-test done");

  return ESP_OK;
}

esp_err_t bb_motor_trigger(bb_motor_pattern_t pattern) {
  if (!BBCLAW_MOTOR_ENABLE) {
    return ESP_OK;
  }
  if (!s_ready || s_queue == NULL) {
    return ESP_ERR_INVALID_STATE;
  }

  motor_event_t evt = {0};
  switch (pattern) {
    case BB_MOTOR_PATTERN_PTT_PRESS:
      evt.on_ms = BBCLAW_MOTOR_PULSE_SHORT_MS;
      evt.off_ms = BBCLAW_MOTOR_PULSE_GAP_MS;
      evt.repeat = 1;
      break;
    case BB_MOTOR_PATTERN_PTT_RELEASE:
      evt.on_ms = BBCLAW_MOTOR_PULSE_RELEASE_MS;
      evt.off_ms = BBCLAW_MOTOR_PULSE_GAP_MS;
      evt.repeat = 1;
      break;
    case BB_MOTOR_PATTERN_TASK_NOTIFY:
      evt.on_ms = BBCLAW_MOTOR_PULSE_SHORT_MS;
      evt.off_ms = BBCLAW_MOTOR_PULSE_GAP_MS;
      evt.repeat = 2;
      break;
    case BB_MOTOR_PATTERN_ERROR_ALERT:
      evt.on_ms = BBCLAW_MOTOR_PULSE_LONG_MS;
      evt.off_ms = BBCLAW_MOTOR_PULSE_GAP_MS;
      evt.repeat = 1;
      break;
    default:
      return ESP_ERR_INVALID_ARG;
  }

  if (xQueueSend(s_queue, &evt, 0) != pdTRUE) {
    ESP_LOGW(TAG, "motor queue full, drop pattern=%d", (int)pattern);
    return ESP_ERR_TIMEOUT;
  }
  return ESP_OK;
}
