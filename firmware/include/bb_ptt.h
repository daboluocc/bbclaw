#pragma once

#include "esp_err.h"

typedef void (*bb_ptt_callback_t)(int pressed);

esp_err_t bb_ptt_init(int gpio_num, bb_ptt_callback_t callback);
int bb_ptt_is_pressed(void);

/* Test hook: simulate a PTT press (1) / release (0) without touching the GPIO.
 * Fires the registered callback exactly like a debounced edge would. Used by
 * the UART debug command channel (bb_uart_cmd) for host-driven self-testing. */
void bb_ptt_inject(int pressed);
