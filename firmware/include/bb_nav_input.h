#pragma once

#include "esp_err.h"

typedef enum {
  BB_NAV_EVENT_ROTATE_CCW = 0,
  BB_NAV_EVENT_ROTATE_CW,
  BB_NAV_EVENT_CLICK,
  BB_NAV_EVENT_LONG_PRESS,
  BB_NAV_EVENT_COUNT,
} bb_nav_event_t;

typedef void (*bb_nav_input_callback_t)(bb_nav_event_t event);

esp_err_t bb_nav_input_init(bb_nav_input_callback_t callback);
