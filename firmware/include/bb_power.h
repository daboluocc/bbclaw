#pragma once

#include "esp_err.h"

typedef struct {
  int supported;
  int available;
  int millivolts;
  int percent;
  int low;
} bb_power_state_t;

esp_err_t bb_power_init(void);
esp_err_t bb_power_refresh(void);
void bb_power_get_state(bb_power_state_t* out_state);
