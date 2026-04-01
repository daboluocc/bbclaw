#pragma once

#include "esp_err.h"

typedef enum {
  BB_LED_IDLE = 0,
  BB_LED_RECORDING = 1,
  BB_LED_PROCESSING = 2,
  BB_LED_REPLY = 3,
  BB_LED_NOTIFICATION = 4,
  BB_LED_SUCCESS = 5,
  BB_LED_ERROR = 6,
} bb_led_status_t;

esp_err_t bb_led_init(void);
esp_err_t bb_led_set_status(bb_led_status_t status);
