#pragma once

#include "esp_err.h"

/* UART debug command channel.
 *
 * Reads newline-terminated ASCII commands off the console UART (UART0, the
 * same port that carries ESP_LOG output) and injects navigation / PTT events
 * into the running firmware. This gives a host with ONLY the UART bridge
 * (e.g. the bench board, where the ESP32-S3 native USB / TinyUSB CDC1 monitor
 * port is not wired) a closed loop for automated button self-testing:
 * write a command, read the resulting log, measure response.
 *
 * Gated by CONFIG_BBCLAW_DEVICE_MONITOR (development builds only). When that
 * option is off, bb_uart_cmd_start() is a no-op.
 *
 * Commands (case-insensitive, one per line):
 *   key up|down|left|right|ok|back|ok-long   inject a nav event
 *   ptt down | ptt up                        inject a raw PTT edge
 *   ptt tap [ms]                             press, hold ms (default 200), release
 *   help                                     list commands
 */
esp_err_t bb_uart_cmd_start(void);
