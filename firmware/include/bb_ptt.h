#pragma once

#include "esp_err.h"

typedef void (*bb_ptt_callback_t)(int pressed);

esp_err_t bb_ptt_init(int gpio_num, bb_ptt_callback_t callback);
int bb_ptt_is_pressed(void);
