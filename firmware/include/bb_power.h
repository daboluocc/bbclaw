#pragma once

#include "esp_err.h"

typedef struct {
  int supported;
  int available;
  int millivolts;
  int percent;
  int low;
  int charging; /* 1 = USB power detected; 0 = on battery (or unknown) */
} bb_power_state_t;

esp_err_t bb_power_init(void);
esp_err_t bb_power_refresh(void);
void bb_power_get_state(bb_power_state_t* out_state);
/* Returns 1 if the device is currently charging, 0 otherwise.
 * Always returns 0 until VBUS GPIO detection is wired up. */
int bbclaw_power_is_charging(void);
