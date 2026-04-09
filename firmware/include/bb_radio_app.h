#pragma once

#include "esp_err.h"

typedef enum {
  BBCLAW_STATE_LOCKED = 0,
  BBCLAW_STATE_UNLOCKED = 1,
} bb_radio_app_state_t;

esp_err_t bb_radio_app_start(void);
